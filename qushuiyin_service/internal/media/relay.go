package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Converted                  bool
	Size                       int64
	Expires                    time.Time
}

var ErrSourceExpired = errors.New("media source expired")

type Relay struct {
	Domains            Domains
	DB                 *sql.DB
	Client             outbound.Client
	Settings           config.Values
	CurrentSettings    func() config.Values
	Key                []byte
	Directory, FFprobe string
	mu                 sync.Mutex
	pending            map[string]chan struct{}
	active             map[string]int
	deleting           map[string]bool
	usageLoaded        bool
	loadingUsage       chan struct{}
	usageIdle          chan struct{}
	usageChanges       int
	usageRevision      uint64
	used, reserved     int64
	imageSlots         chan struct{}
}

func (r *Relay) settings() config.Values {
	if r.CurrentSettings != nil {
		return r.CurrentSettings()
	}
	return r.Settings
}

type FileIdentity struct {
	Platform, ContentID       string
	UserID, ResultID, AssetID string
	AssetVersion              int64
}

func (r *Relay) shareKey(id FileIdentity, version string, a parse.Asset) string {
	if id.ContentID != "" {
		return auth.Digest(r.Key, id.Platform+":"+id.ContentID+":"+a.Role+":"+strconv.Itoa(a.Order)+":"+version)
	}
	parts := []string{id.UserID, id.ResultID, id.AssetID, strconv.FormatInt(id.AssetVersion, 10), version}
	raw, _ := json.Marshal(parts)
	return auth.Digest(r.Key, "private-media:"+string(raw))
}

// Only this module chooses paths. The identity comes from the authorized result.
func (r *Relay) Prepare(ctx context.Context, id FileIdentity, version string, a parse.Asset) (Prepared, error) {
	settings := r.settings()
	ctx, cancel := context.WithTimeout(ctx, settings.Seconds("media_fetch_timeout_seconds"))
	defer cancel()
	return r.prepare(ctx, ctx, id, version, a, false, settings)
}

func (r *Relay) PrepareThumbnail(ctx, queued context.Context, id FileIdentity, version string, a parse.Asset) (Prepared, error) {
	settings := r.settings()
	edge := strconv.FormatInt(settings.Int("thumbnail_max_edge_px"), 10)
	quality := strconv.FormatInt(settings.Int("thumbnail_jpeg_quality"), 10)
	return r.prepare(ctx, queued, id, "thumb-"+edge+"-"+quality+"-"+version, a, true, settings)
}

func (r *Relay) prepare(ctx, queued context.Context, id FileIdentity, version string, a parse.Asset, thumbnail bool, settings config.Values) (Prepared, error) {
	if id.ContentID == "" && (id.UserID == "" || id.ResultID == "" || id.AssetID == "") {
		return Prepared{}, errors.New("missing private media identity")
	}
	share := r.shareKey(id, version, a)
	for {
		if err := queued.Err(); err != nil {
			return Prepared{}, err
		}
		r.mu.Lock()
		if r.pending == nil {
			r.pending = map[string]chan struct{}{}
		}
		if wait := r.pending[share]; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-queued.Done():
				return Prepared{}, queued.Err()
			}
		}
		done := make(chan struct{})
		r.pending[share] = done
		r.mu.Unlock()
		defer func() { r.mu.Lock(); delete(r.pending, share); close(done); r.mu.Unlock() }()
		break
	}
	var imageWait *time.Duration
	if queueDeadline, ok := queued.Deadline(); thumbnail && ok {
		remaining := time.Until(queueDeadline)
		imageWait = &remaining
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Seconds("media_fetch_timeout_seconds"))
	defer cancel()
	var existing Prepared
	err := r.DB.QueryRowContext(ctx, "SELECT id,storage_key,mime_type,size_bytes,sha256,converted,expires_at FROM media_file WHERE share_key=? AND output_version=? AND status='ready' AND expires_at>UTC_TIMESTAMP(3)", share, version).Scan(&existing.ID, &existing.StorageKey, &existing.MIME, &existing.Size, &existing.Hash, &existing.Converted, &existing.Expires)
	if err == nil {
		if f, release, e := r.Acquire(existing.StorageKey); e == nil {
			defer release()
			info, e := f.Stat()
			if e == nil && info.Size() == existing.Size {
				var ready time.Time
				if e = r.DB.QueryRowContext(ctx, "SELECT ready_at FROM media_file WHERE id=?", existing.ID).Scan(&ready); e != nil {
					return Prepared{}, e
				}
				until := time.Now().Add(settings.Seconds("media_cache_ttl_seconds"))
				maximum := ready.Add(settings.Seconds("shared_media_max_age_seconds"))
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
	release, err := r.reserve(ctx, mediaSizeLimit(settings, a.Kind))
	if err != nil {
		return Prepared{}, err
	}
	var releaseFile func()
	defer func() {
		release()
		if releaseFile != nil {
			releaseFile()
		}
	}()
	sources := append([]string{a.Source}, a.Alternates...)
	var last error
	for i, source := range sources {
		if int64(i) >= settings.Int("max_media_candidates") {
			break
		}
		storageKey := auth.ID() + ".media"
		hold, e := r.holdFile(storageKey)
		if e != nil {
			return Prepared{}, e
		}
		file, e := r.fetchOutputTo(ctx, source, a.Kind, storageKey, thumbnail, imageWait, settings)
		if e != nil {
			hold()
			if errors.Is(e, errAnimatedWebP) {
				return Prepared{}, e
			}
			last = e
			continue
		}
		releaseFile = hold
		now := time.Now().UTC()
		file.ID = auth.ID()
		file.Expires = now.Add(settings.Seconds("media_cache_ttl_seconds"))
		// Expired cache rows lose their sharing key; immutable old files can still
		// be retained until their active authorizations and transfers have ended.
		tx, e := r.DB.BeginTx(ctx, nil)
		if e != nil {
			_ = os.Remove(filepath.Join(r.Directory, file.StorageKey))
			return Prepared{}, e
		}
		_, e = tx.ExecContext(ctx, "UPDATE media_file SET share_key=NULL WHERE share_key=? AND output_version=?", share, version)
		if e == nil {
			_, e = tx.ExecContext(ctx, "INSERT INTO media_file(id,share_key,output_version,storage_key,status,mime_type,size_bytes,sha256,converted,ready_at,expires_at,last_access_at) VALUES (?,?,?,?,'ready',?,?,?,?,?,?,?)", file.ID, share, version, file.StorageKey, file.MIME, file.Size, file.Hash, file.Converted, now, file.Expires, now)
		}
		if e != nil {
			tx.Rollback()
			_ = os.Remove(filepath.Join(r.Directory, file.StorageKey))
			return Prepared{}, e
		}
		finish := r.beginUsageChange()
		e = tx.Commit()
		if e != nil {
			finish(0, false)
			// A lost commit acknowledgment can still leave a ready row. Keep its
			// file intact; the orphan sweep handles a transaction that rolled back.
			_ = r.calibrateUsage(context.WithoutCancel(ctx))
			return Prepared{}, e
		}
		finish(file.Size, true)
		return file, nil
	}
	if last == nil {
		last = errors.New("no media candidates")
	}
	return Prepared{}, last
}

func validStorageKey(key string) bool {
	return filepath.Base(key) == key && !strings.ContainsAny(key, "/\\:") && strings.HasSuffix(key, ".media")
}

func (r *Relay) Open(key string) (*os.File, error) {
	if !validStorageKey(key) {
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
	return r.fetchOutput(ctx, source, kind, false)
}

func (r *Relay) fetchOutput(ctx context.Context, source, kind string, thumbnail bool) (Prepared, error) {
	return r.fetchOutputTo(ctx, source, kind, auth.ID()+".media", thumbnail, nil, r.settings())
}

func mediaSizeLimit(settings config.Values, kind string) int64 {
	if kind == "video" {
		return settings.Int("relay_max_video_bytes")
	}
	return settings.Int("max_image_bytes")
}

func relaySizeError(settings config.Values, kind string, size int64) error {
	if kind == "video" && size <= settings.Int("max_video_bytes") {
		return errors.New("DIRECT_DOWNLOAD_UNAVAILABLE")
	}
	return errors.New("FILE_TOO_LARGE")
}

func (r *Relay) fetchOutputTo(ctx context.Context, source, kind, key string, thumbnail bool, imageWait *time.Duration, settings config.Values) (Prepared, error) {
	var out Prepared
	resp, err := r.Client.Headers(ctx, source, http.MethodGet, "", func(u *url.URL) bool { return outbound.HTTPS(u) })
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == 403 || resp.StatusCode == 404 || resp.StatusCode == 410 {
			return out, ErrSourceExpired
		}
		return out, errors.New("media source unavailable")
	}
	limit := mediaSizeLimit(settings, kind)
	if limit <= 0 || resp.ContentLength > limit {
		return out, relaySizeError(settings, kind, resp.ContentLength)
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
	if n == 0 {
		return out, errors.New("MEDIA_INVALID")
	}
	if n > limit {
		return out, relaySizeError(settings, kind, max(n, resp.ContentLength))
	}
	if resp.ContentLength >= 0 && resp.ContentLength != n {
		return out, errors.New("incomplete media")
	}
	if _, err = part.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	actual := sniffMedia(part, 12)
	if kind == "video" && actual != "video/mp4" || kind != "video" && actual != "image/jpeg" && actual != "image/png" && actual != "image/webp" {
		return out, errors.New("FORMAT_UNSUPPORTED")
	}
	converted := actual == "image/webp"
	declared, generic := contentType(resp)
	if !generic && declared != actual {
		return out, errors.New("media MIME mismatch")
	}
	if kind == "video" {
		if err = r.validateVideo(ctx, path); err != nil {
			return out, err
		}
	} else {
		actual, err = r.processImage(ctx, part, actual, thumbnail, imageWait, settings)
		if err != nil {
			return out, err
		}
	}
	// Hash and byte count always describe the final immutable output.
	if _, err = part.Seek(0, io.SeekStart); err != nil {
		return out, err
	}
	digest.Reset()
	n, err = io.Copy(digest, io.LimitReader(part, limit+1))
	if err != nil || n == 0 || n > limit {
		return out, errors.New("FILE_TOO_LARGE")
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	if err = part.Sync(); err != nil {
		return out, err
	}
	if err = part.Close(); err != nil {
		return out, err
	}
	if err = os.Rename(path, filepath.Join(r.Directory, key)); err != nil {
		return out, err
	}
	return Prepared{StorageKey: key, MIME: actual, Size: n, Hash: hex.EncodeToString(digest.Sum(nil)), Converted: converted}, nil
}

type limitedProbeOutput struct {
	bytes []byte
	limit int
}

// Commit and deletion can overlap. Reject a SUM snapshot if either crossed
// its read, so a registration or deletion is never counted twice.
func (r *Relay) beginUsageChange() func(int64, bool) {
	r.mu.Lock()
	if r.usageChanges == 0 {
		r.usageIdle = make(chan struct{})
	}
	r.usageChanges++
	r.usageRevision++
	r.mu.Unlock()
	return func(delta int64, confirmed bool) {
		r.mu.Lock()
		if confirmed {
			r.used += delta
		} else {
			r.usageLoaded = false
		}
		r.usageChanges--
		if r.usageChanges == 0 {
			close(r.usageIdle)
			r.usageIdle = nil
		}
		r.mu.Unlock()
	}
}

func (r *Relay) refreshUsage(ctx context.Context, force bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.mu.Lock()
		if r.usageLoaded && !force {
			r.mu.Unlock()
			return nil
		}
		wait := r.loadingUsage
		if wait == nil {
			wait = r.usageIdle
		}
		if wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		r.loadingUsage = done
		revision := r.usageRevision
		r.mu.Unlock()

		var used int64
		err := r.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(size_bytes),0) FROM media_file").Scan(&used)

		r.mu.Lock()
		stable := r.usageRevision == revision
		if err == nil && stable {
			r.used = used
			r.usageLoaded = true
		}
		r.loadingUsage = nil
		close(done)
		r.mu.Unlock()
		if err != nil || stable {
			return err
		}
	}
}

func (r *Relay) calibrateUsage(ctx context.Context) error {
	check, cancel := context.WithTimeout(ctx, r.settings().Seconds("database_operation_timeout_seconds"))
	defer cancel()
	return r.refreshUsage(check, true)
}

func (r *Relay) reserve(ctx context.Context, limit int64) (func(), error) {
	for {
		if err := r.refreshUsage(ctx, false); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.usageLoaded {
			break
		}
		r.mu.Unlock()
	}
	defer r.mu.Unlock()
	if limit <= 0 || limit > r.settings().Int("media_cache_budget_bytes")-r.used-r.reserved {
		return nil, errors.New("SERVICE_BUSY")
	}
	r.reserved += limit
	return func() {
		r.mu.Lock()
		r.reserved -= limit
		r.mu.Unlock()
	}, nil
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
	output := &limitedProbeOutput{limit: int(r.settings().Int("ffprobe_max_output_bytes"))}
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
