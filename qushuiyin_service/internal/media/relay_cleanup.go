package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (r *Relay) Acquire(key string) (*os.File, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.Open(key)
	if err != nil {
		return nil, nil, err
	}
	if r.active == nil {
		r.active = map[string]int{}
	}
	r.active[key]++
	release := sync.OnceFunc(func() {
		f.Close()
		r.mu.Lock()
		r.active[key]--
		if r.active[key] == 0 {
			delete(r.active, key)
		}
		r.mu.Unlock()
	})
	return f, release, nil
}
func (r *Relay) Clean(ctx context.Context) {
	ticker := time.NewTicker(r.Settings.Seconds("media_gc_interval_seconds"))
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
func (r *Relay) CleanOnce(ctx context.Context) {
	rows, err := r.DB.QueryContext(ctx, `SELECT f.id,f.storage_key FROM media_file f WHERE f.expires_at<=UTC_TIMESTAMP(3) AND NOT EXISTS (SELECT 1 FROM media_object o WHERE o.file_id=f.id AND o.status='ready' AND o.expires_at>UTC_TIMESTAMP(3)) LIMIT ?`, r.Settings.Int("pagination_max"))
	if err != nil {
		return
	}
	type entry struct{ id, key string }
	var pending []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.id, &e.key) != nil {
			rows.Close()
			return
		}
		pending = append(pending, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, e := range pending {
		if filepath.Base(e.key) != e.key || strings.ContainsAny(e.key, "/\\:") || !strings.HasSuffix(e.key, ".media") {
			continue
		}
		r.mu.Lock()
		if r.active[e.key] > 0 {
			r.mu.Unlock()
			continue
		}
		changed, err := r.DB.ExecContext(ctx, `UPDATE media_file f SET status='deleting',version=version+1 WHERE f.id=? AND f.expires_at<=UTC_TIMESTAMP(3) AND NOT EXISTS (SELECT 1 FROM media_object o WHERE o.file_id=f.id AND o.status='ready' AND o.expires_at>UTC_TIMESTAMP(3))`, e.id)
		if err == nil {
			n, _ := changed.RowsAffected()
			if n > 0 {
				err = os.Remove(filepath.Join(r.Directory, e.key))
				if err == nil || errors.Is(err, os.ErrNotExist) {
					_, _ = r.DB.ExecContext(ctx, "DELETE FROM media_file WHERE id=? AND status='deleting'", e.id)
				}
			}
		}
		r.mu.Unlock()
	}
	// Incomplete files are never downloadable and have a bounded stale lifetime.
	files, err := os.ReadDir(r.Directory)
	if err != nil {
		return
	}
	before := time.Now().Add(-r.Settings.Seconds("media_fetch_timeout_seconds") - r.Settings.Seconds("lease_cleanup_grace_seconds"))
	for _, f := range files {
		if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".part") {
			info, e := f.Info()
			if e == nil && info.ModTime().Before(before) {
				_ = os.Remove(filepath.Join(r.Directory, f.Name()))
			}
		}
		if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".media") {
			info, e := f.Info()
			if e != nil || !info.ModTime().Before(before) {
				continue
			}
			r.mu.Lock()
			var count int
			if r.active[f.Name()] == 0 && r.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM media_file WHERE storage_key=?", f.Name()).Scan(&count) == nil && count == 0 {
				_ = os.Remove(filepath.Join(r.Directory, f.Name()))
			}
			r.mu.Unlock()
		}
	}
}
