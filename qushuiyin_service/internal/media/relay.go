package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
)

type Prepared struct {
	ID, StorageKey, MIME, Hash string
	Size                       int64
	Expires                    time.Time
}
type Relay struct {
	DB                 *sql.DB
	Client             outbound.Client
	Settings           config.Values
	Key                []byte
	Directory, FFprobe string
	mu                 sync.Mutex
	pending            map[string]chan struct{}
	active             map[string]int
	reserved           int64
}

// Only this module chooses paths. Storage keys and source candidates never
// originate in download requests. Each caller is a bounded media worker.
func (r *Relay) Prepare(ctx context.Context, platform, contentID, version string, a parse.Asset) (Prepared, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Settings.Seconds("media_fetch_timeout_seconds"))
	defer cancel()
	share := auth.Digest(r.Key, platform+":"+contentID+":"+a.Role+":"+strconv.Itoa(a.Order)+":"+version)
	if contentID == "" {
		share = auth.ID()
	}
	for {
		r.mu.Lock()
		if r.pending == nil {
			r.pending = map[string]chan struct{}{}
		}
		if wait := r.pending[share]; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return Prepared{}, ctx.Err()
			}
		}
		done := make(chan struct{})
		r.pending[share] = done
		r.mu.Unlock()
		defer func() { r.mu.Lock(); delete(r.pending, share); close(done); r.mu.Unlock() }()
		break
	}
	var existing Prepared
	err := r.DB.QueryRowContext(ctx, "SELECT id,storage_key,mime_type,size_bytes,sha256,expires_at FROM media_file WHERE share_key=? AND output_version=? AND status='ready' AND expires_at>UTC_TIMESTAMP(3)", share, version).Scan(&existing.ID, &existing.StorageKey, &existing.MIME, &existing.Size, &existing.Hash, &existing.Expires)
	if err == nil {
		if f, e := r.Open(existing.StorageKey); e == nil {
			info, e := f.Stat()
			f.Close()
			if e == nil && info.Size() == existing.Size {
				var ready time.Time
				if e = r.DB.QueryRowContext(ctx, "SELECT ready_at FROM media_file WHERE id=?", existing.ID).Scan(&ready); e != nil {
					return Prepared{}, e
				}
				until := time.Now().Add(r.Settings.Seconds("media_cache_ttl_seconds"))
				maximum := ready.Add(r.Settings.Seconds("shared_media_max_age_seconds"))
				if until.After(maximum) {
					until = maximum
				}
				if until.After(existing.Expires) {
					if _, e = r.DB.ExecContext(ctx, "UPDATE media_file SET expires_at=?,last_access_at=UTC_TIMESTAMP(3) WHERE id=? AND status='ready'", until, existing.ID); e != nil {
						return Prepared{}, e
					}
					existing.Expires = until
				}
				return existing, nil
			}
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Prepared{}, err
	}
	release, err := r.reserve(ctx, a.Kind)
	if err != nil {
		return Prepared{}, err
	}
	defer release()
	sources := append([]string{a.Source}, a.Alternates...)
	var last error
	for i, source := range sources {
		if int64(i) >= r.Settings.Int("max_media_candidates") {
			break
		}
		file, e := r.fetch(ctx, source, a.Kind)
		if e != nil {
			last = e
			continue
		}
		now := time.Now().UTC()
		file.ID = auth.ID()
		file.Expires = now.Add(r.Settings.Seconds("media_cache_ttl_seconds"))
		// Expired cache rows lose their sharing key; immutable old files can still
		// be retained until their active authorizations and transfers have ended.
		tx, e := r.DB.BeginTx(ctx, nil)
		if e != nil {
			_ = os.Remove(filepath.Join(r.Directory, file.StorageKey))
			return Prepared{}, e
		}
		_, e = tx.ExecContext(ctx, "UPDATE media_file SET share_key=NULL WHERE share_key=? AND output_version=?", share, version)
		if e == nil {
			_, e = tx.ExecContext(ctx, "INSERT INTO media_file(id,share_key,output_version,storage_key,status,mime_type,size_bytes,sha256,ready_at,expires_at,last_access_at) VALUES (?,?,?,?,'ready',?,?,?,?,?,?)", file.ID, share, version, file.StorageKey, file.MIME, file.Size, file.Hash, now, file.Expires, now)
		}
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
		if e != nil {
			_ = os.Remove(filepath.Join(r.Directory, file.StorageKey))
			return Prepared{}, e
		}
		return file, nil
	}
	if last == nil {
		last = errors.New("no media candidates")
	}
	return Prepared{}, last
}

func (r *Relay) Open(key string) (*os.File, error) {
	if filepath.Base(key) != key || strings.ContainsAny(key, "/\\:") || !strings.HasSuffix(key, ".media") {
		return nil, errors.New("invalid media storage key")
	}
	path := filepath.Join(r.Directory, key)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("invalid media file")
	}
	return os.Open(path)
}
func (r *Relay) fetch(ctx context.Context, source, kind string) (Prepared, error) {
	var out Prepared
	resp, err := r.Client.Headers(ctx, source, http.MethodGet, "", func(u *url.URL) bool { return outbound.HTTPS(u) })
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, errors.New("media source unavailable")
	}
	limit := r.Settings.Int("max_image_bytes")
	if kind == "video" {
		limit = r.Settings.Int("relay_max_video_bytes")
	}
	if limit <= 0 || resp.ContentLength > limit {
		return out, errors.New("FILE_TOO_LARGE")
	}
	if err = os.MkdirAll(r.Directory, 0700); err != nil {
		return out, err
	}
	part, err := os.CreateTemp(r.Directory, "*.part")
	if err != nil {
		return out, err
	}
	path := part.Name()
	defer func() { part.Close(); os.Remove(path) }()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(part, digest), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return out, err
	}
	if n == 0 || n > limit {
		return out, errors.New("FILE_TOO_LARGE")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != n {
		return out, errors.New("incomplete media")
	}
	if _, err = part.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	actual := sniffMedia(part, 12)
	if kind == "video" {
		if actual != "video/mp4" {
			return out, errors.New("FORMAT_UNSUPPORTED")
		}
		if err = r.validateVideo(ctx, path); err != nil {
			return out, err
		}
	} else {
		if actual != "image/jpeg" && actual != "image/png" {
			return out, errors.New("FORMAT_UNSUPPORTED")
		}
		if _, err = part.Seek(0, io.SeekStart); err != nil {
			return out, err
		}
		cfg, _, e := image.DecodeConfig(part)
		if e != nil {
			return out, e
		}
		if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > r.Settings.Int("max_image_pixels")/int64(cfg.Height) {
			return out, errors.New("image pixel limit")
		}
		if _, err = part.Seek(0, io.SeekStart); err != nil {
			return out, err
		}
		if _, _, err = image.Decode(part); err != nil {
			return out, err
		}
	}
	// A declared non-generic MIME must agree with the actual file format.
	declared, generic := contentType(resp)
	if !generic && declared != actual {
		return out, errors.New("media MIME mismatch")
	}
	if err = part.Sync(); err != nil {
		return out, err
	}
	if err = part.Close(); err != nil {
		return out, err
	}
	key := auth.ID() + ".media"
	if err = os.Rename(path, filepath.Join(r.Directory, key)); err != nil {
		return out, err
	}
	return Prepared{StorageKey: key, MIME: actual, Size: n, Hash: hex.EncodeToString(digest.Sum(nil))}, nil
}

type limitedProbeOutput struct {
	bytes []byte
	limit int
}

func (r *Relay) reserve(ctx context.Context, kind string) (func(), error) {
	limit := r.Settings.Int("max_image_bytes")
	if kind == "video" {
		limit = r.Settings.Int("relay_max_video_bytes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var used int64
	if err := r.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(size_bytes),0) FROM media_file").Scan(&used); err != nil {
		return nil, err
	}
	if limit <= 0 || used+r.reserved+limit > r.Settings.Int("media_cache_budget_bytes") {
		return nil, errors.New("SERVICE_BUSY")
	}
	r.reserved += limit
	return sync.OnceFunc(func() { r.mu.Lock(); r.reserved -= limit; r.mu.Unlock() }), nil
}

func (b *limitedProbeOutput) Write(p []byte) (int, error) {
	if len(b.bytes)+len(p) > b.limit {
		return 0, errors.New("probe output limit")
	}
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}
func (r *Relay) validateVideo(ctx context.Context, path string) error {
	// Local files only. Explicit protocol restrictions keep malformed containers
	// from making ffprobe fetch remote playlists or secondary resources.
	cmd := exec.CommandContext(ctx, r.FFprobe, "-v", "error", "-protocol_whitelist", "file,pipe", "-show_entries", "format=format_name:stream=codec_type,codec_name", "-of", "json", path)
	output := &limitedProbeOutput{limit: int(r.Settings.Int("direct_probe_range_bytes"))}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("FORMAT_UNSUPPORTED")
	}
	var info struct {
		Format struct {
			FormatName string `json:"format_name"`
		}
		Streams []struct {
			Type  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		}
	}
	if json.Unmarshal(output.bytes, &info) != nil || !strings.Contains(info.Format.FormatName, "mp4") {
		return errors.New("FORMAT_UNSUPPORTED")
	}
	videos := 0
	for _, stream := range info.Streams {
		switch stream.Type {
		case "video":
			if stream.Codec != "h264" {
				return errors.New("FORMAT_UNSUPPORTED")
			}
			videos++
		case "audio":
			if stream.Codec != "aac" {
				return errors.New("FORMAT_UNSUPPORTED")
			}
		default:
			return errors.New("FORMAT_UNSUPPORTED")
		}
	}
	if videos != 1 {
		return errors.New("FORMAT_UNSUPPORTED")
	}
	return nil
}
