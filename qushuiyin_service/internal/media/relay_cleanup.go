package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (r *Relay) holdFile(key string) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleting[key] {
		return nil, os.ErrNotExist
	}
	if r.active == nil {
		r.active = map[string]int{}
	}
	r.active[key]++
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.active[key]--
		if r.active[key] == 0 {
			delete(r.active, key)
		}
	}, nil
}

func (r *Relay) Acquire(key string) (*os.File, func(), error) {
	release, err := r.holdFile(key)
	if err != nil {
		return nil, nil, err
	}
	f, err := r.Open(key)
	if err != nil {
		release()
		return nil, nil, err
	}
	return f, sync.OnceFunc(func() {
		f.Close()
		release()
	}), nil
}

func (r *Relay) claimRemoval(key string) bool {
	if !validStorageKey(key) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[key] > 0 || r.deleting[key] {
		return false
	}
	if r.deleting == nil {
		r.deleting = map[string]bool{}
	}
	r.deleting[key] = true
	return true
}

func (r *Relay) finishRemoval(key string) {
	r.mu.Lock()
	delete(r.deleting, key)
	r.mu.Unlock()
}

func (r *Relay) Clean(ctx context.Context) {
	ticker := time.NewTicker(r.settings().Seconds("media_gc_interval_seconds"))
	defer ticker.Stop()
	for {
		r.CleanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (r *Relay) volumeHeadroom() (int64, error) {
	settings := r.settings()
	if _, ok := settings.Data["disk_high_watermark_percent"]; !ok {
		return math.MaxInt64, nil
	}
	percent := settings.Int("disk_high_watermark_percent")
	getSpace := r.volume
	if getSpace == nil {
		getSpace = volumeSpace
	}
	free, total, err := getSpace(r.Directory)
	if err != nil || total == 0 {
		return 0, ErrStorageCapacity
	}
	headroom := float64(free) - float64(total)*float64(100-percent)/100
	return int64(min(float64(math.MaxInt64-1024), max(0, headroom))), nil
}

func (r *Relay) CleanOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, r.settings().Seconds("database_operation_timeout_seconds"))
	defer cancel()
	room, diskErr := r.volumeHeadroom()
	if diskErr != nil {
		slog.Warn("media volume inspection failed", "event", "media_storage_unavailable")
	}
	pressure := diskErr == nil && room == 0
	if pressure {
		slog.Warn("media volume above high watermark", "event", "media_disk_high_watermark")
	}
	if err := r.calibrateUsage(ctx); err != nil {
		slog.Warn("media cache calibration failed", "event", "media_cleanup_failed")
		return
	}
	limit := int(r.settings().Int("cleanup_batch_size"))
	if limit <= 0 {
		return
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT f.id,f.storage_key,f.size_bytes FROM media_file f WHERE (f.expires_at<=UTC_TIMESTAMP(3) OR f.status='deleting' OR ?) AND NOT EXISTS (SELECT 1 FROM media_object o WHERE o.file_id=f.id AND o.status='ready' AND o.expires_at>UTC_TIMESTAMP(3)) ORDER BY f.expires_at,f.id LIMIT ?`, pressure, limit)
	if err != nil {
		return
	}
	var pending []expiredFile
	for rows.Next() {
		var f expiredFile
		if rows.Scan(&f.id, &f.key, &f.size) != nil {
			rows.Close()
			return
		}
		pending = append(pending, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, f := range pending {
		r.cleanExpiredFile(ctx, f, pressure)
	}

	dir, err := os.Open(r.Directory)
	if err != nil {
		return
	}
	defer dir.Close()
	before := time.Now().Add(-r.settings().Seconds("media_fetch_timeout_seconds") - r.settings().Seconds("lease_cleanup_grace_seconds"))
	for ctx.Err() == nil {
		files, err := dir.ReadDir(limit)
		if err != nil && !errors.Is(err, io.EOF) {
			slog.Warn("media directory scan failed", "event", "media_cleanup_failed")
			return
		}
		r.cleanDiskEntries(ctx, files, before)
		if errors.Is(err, io.EOF) {
			return
		}
	}
}

func (r *Relay) cleanDiskEntries(ctx context.Context, files []os.DirEntry, before time.Time) {
	var candidates []string
	for _, f := range files {
		if !f.Type().IsRegular() {
			continue
		}
		key := f.Name()
		if !strings.HasSuffix(key, ".part") && !validStorageKey(key) {
			continue
		}
		info, e := f.Info()
		if e != nil || !info.ModTime().Before(before) {
			continue
		}
		if strings.HasSuffix(key, ".part") {
			_ = os.Remove(filepath.Join(r.Directory, key))
		} else {
			candidates = append(candidates, key)
		}
	}
	if len(candidates) > 0 {
		r.cleanOrphans(ctx, candidates)
	}
}

type expiredFile struct {
	id, key string
	size    int64
}

func (r *Relay) cleanExpiredFile(ctx context.Context, f expiredFile, pressure bool) {
	if !r.claimRemoval(f.key) {
		return
	}
	defer r.finishRemoval(f.key)
	changed, err := r.DB.ExecContext(ctx, `UPDATE media_file f SET status='deleting',version=version+1 WHERE f.id=? AND (f.expires_at<=UTC_TIMESTAMP(3) OR f.status='deleting' OR ?) AND NOT EXISTS (SELECT 1 FROM media_object o WHERE o.file_id=f.id AND o.status='ready' AND o.expires_at>UTC_TIMESTAMP(3))`, f.id, pressure)
	if err != nil {
		return
	}
	if n, err := changed.RowsAffected(); err != nil || n != 1 {
		return
	}
	err = os.Remove(filepath.Join(r.Directory, f.key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("media cache file removal failed", "event", "media_cleanup_failed")
		return
	}
	finish := r.beginUsageChange()
	deleted, err := r.DB.ExecContext(ctx, "DELETE FROM media_file WHERE id=? AND status='deleting'", f.id)
	if err != nil {
		finish(0, false)
		_ = r.calibrateUsage(context.WithoutCancel(ctx))
		return
	}
	n, err := deleted.RowsAffected()
	finish(-n*f.size, err == nil)
	if err != nil {
		_ = r.calibrateUsage(context.WithoutCancel(ctx))
	}
}

func (r *Relay) cleanOrphans(ctx context.Context, keys []string) {
	registered, err := r.registeredFiles(ctx, keys)
	if err != nil {
		return
	}
	var claimed []string
	for _, key := range keys {
		if !registered[key] && r.claimRemoval(key) {
			defer r.finishRemoval(key)
			claimed = append(claimed, key)
		}
	}
	if len(claimed) == 0 {
		return
	}
	// A publisher could have committed between the first lookup and the claim.
	registered, err = r.registeredFiles(ctx, claimed)
	if err != nil {
		return
	}
	for _, key := range claimed {
		if !registered[key] {
			slog.Warn("orphaned media file detected", "event", "media_orphan_removed")
			_ = os.Remove(filepath.Join(r.Directory, key))
		}
	}
}

func (r *Relay) registeredFiles(ctx context.Context, keys []string) (map[string]bool, error) {
	args := make([]any, len(keys))
	for i, key := range keys {
		args[i] = key
	}
	query := fmt.Sprintf("SELECT storage_key FROM media_file WHERE storage_key IN (%s?)", strings.Repeat("?,", len(args)-1))
	rows, err := r.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	registered := make(map[string]bool, len(args))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		registered[key] = true
	}
	return registered, rows.Err()
}
