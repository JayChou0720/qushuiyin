package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"qushuiyin/service/internal/auth"
)

func (s *Mock) deleteData(w http.ResponseWriter, r *http.Request, c credential) {
	ctx := r.Context()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	defer tx.Rollback()
	if err = s.lockUser(ctx, tx, c); err != nil {
		privateWriteError(w, err)
		return
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	deadline := now.Add(time.Duration(s.Settings.Int("cleanup_completion_hours")) * time.Hour)
	_, err = tx.ExecContext(ctx, "UPDATE app_user SET status='deleted',deleted_at=?,subject_hash=NULL WHERE id=?", now, c.User)
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE app_session SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now, c.User)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM feedback WHERE user_id=?", c.User)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM event_ingest WHERE user_id=?", c.User)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO cleanup_job(id,user_id,kind,status,deadline_at) VALUES (?,?,'user_data','PENDING',?)", auth.ID(), c.User, deadline)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		privateWriteError(w, err)
		return
	}
	s.evictUser(c.User)
	ok(w, 200, dto{"accessRevoked": true, "cleanupStatus": "scheduled", "cleanupDeadlineAt": deadline})
}

func (s *Mock) evictUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, x := range s.jobs {
		if x.User == user {
			x.cancel()
			delete(s.jobs, id)
		}
	}
	for id, x := range s.results {
		if x.User == user {
			delete(s.results, id)
		}
	}
	for id, x := range s.batches {
		if x.User == user {
			delete(s.batches, id)
		}
	}
	for id, x := range s.previews {
		if x.user == user {
			delete(s.previews, id)
		}
	}
	for k := range s.keys {
		if strings.HasPrefix(k, user+":") {
			delete(s.keys, k)
		}
	}
	for _, prefix := range []string{"parse:", "ticket:", "copy:"} {
		delete(s.rates, prefix+user)
	}
}

// Mock fixtures are shared immutable samples. Only each user's objects and references are deleted.
var cleanupUserQueries = []string{
	"DELETE FROM save_batch_item WHERE user_id=?",
	"DELETE FROM save_batch WHERE user_id=?",
	"DELETE FROM media_object WHERE user_id=?",
	"DELETE FROM media_asset WHERE user_id=?",
	"DELETE FROM parse_result WHERE user_id=?",
	"DELETE FROM parse_job WHERE user_id=?",
	"DELETE FROM feedback WHERE user_id=?",
	"DELETE FROM event_ingest WHERE user_id=?",
	"DELETE FROM idempotency_record WHERE user_id=?",
	"DELETE FROM app_session WHERE user_id=?",
	"DELETE FROM usage_counter WHERE subject_key=?",
}

func (s *Mock) runCleanup(ctx context.Context, id string) error {
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var user string
	err = tx.QueryRowContext(ctx, "SELECT j.user_id FROM cleanup_job j JOIN app_user u ON u.id=j.user_id WHERE j.id=? AND j.kind='user_data' AND j.status='PENDING' AND u.status='deleted' FOR UPDATE SKIP LOCKED", id).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, query := range cleanupUserQueries {
		if _, err = tx.ExecContext(ctx, query, user); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE cleanup_job SET status='SUCCEEDED',attempt=attempt+1,last_error_code=NULL WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Mock) cleanDatabase(ctx context.Context, now time.Time) {
	if s.Sessions.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.Settings.Seconds("database_operation_timeout_seconds"))
	defer cancel()
	db := s.Sessions.DB
	limit := s.Settings.Int("cleanup_batch_size")
	if s.Persistent {
		// Keep opaque IDs for explicit expiry/idempotency responses, but erase
		// source signatures and content as soon as their short retention ends.
		for _, q := range []string{
			"UPDATE save_batch SET state_enc=NULL,version=version+1 WHERE expires_at<=? AND state_enc IS NOT NULL LIMIT ?",
			"UPDATE save_batch_item SET direct_url_enc=NULL,key_version=NULL,version=version+1 WHERE direct_expires_at<=? AND direct_url_enc IS NOT NULL LIMIT ?",
			"UPDATE parse_job SET source_url_enc=NULL,version=version+1 WHERE source_expires_at<=? AND source_url_enc IS NOT NULL LIMIT ?",
			"UPDATE parse_result SET history_url_enc=NULL,title=NULL,description=NULL,author_name=NULL,content_id=NULL WHERE expires_at<=? AND (history_url_enc IS NOT NULL OR title IS NOT NULL OR description IS NOT NULL OR author_name IS NOT NULL OR content_id IS NOT NULL) LIMIT ?",
			"UPDATE media_asset SET source_candidates_enc='',availability='unavailable',reason_code='MEDIA_SOURCE_EXPIRED',version=version+1 WHERE source_expiry<=? AND source_candidates_enc<>'' LIMIT ?",
			"UPDATE media_object SET status='expired',version=version+1 WHERE expires_at<=? AND status='ready' LIMIT ?",
		} {
			if _, err := db.ExecContext(ctx, q, now, limit); err != nil {
				slog.Warn("short-lived media retention sweep failed")
			}
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT id,deadline_at FROM cleanup_job WHERE kind='user_data' AND status='PENDING' ORDER BY updated_at,id LIMIT ?", limit)
	if err != nil {
		slog.Warn("local data cleanup could not read pending jobs")
		return
	}
	type task struct {
		id       string
		deadline time.Time
	}
	pending := []task{}
	for rows.Next() {
		var x task
		if err = rows.Scan(&x.id, &x.deadline); err != nil {
			break
		}
		pending = append(pending, x)
	}
	scanErr := rows.Err()
	rows.Close()
	if err != nil || scanErr != nil {
		slog.Warn("local data cleanup could not load pending jobs")
		return
	}
	for _, x := range pending {
		if !now.Before(x.deadline) {
			slog.Warn("local data cleanup overdue", "cleanup_id", x.id)
		}
		if err = s.runCleanup(ctx, x.id); err != nil {
			slog.Warn("local data cleanup failed; will retry", "cleanup_id", x.id)
			if _, recordErr := db.ExecContext(ctx, "UPDATE cleanup_job SET attempt=attempt+1,last_error_code='INTERNAL_ERROR' WHERE id=? AND status='PENDING'", x.id); recordErr != nil {
				slog.Warn("local data cleanup could not record failure", "cleanup_id", x.id)
			}
		}
	}
	for _, q := range []struct {
		query  string
		cutoff time.Time
	}{
		{"DELETE FROM feedback WHERE expires_at<=? ORDER BY expires_at LIMIT ?", now},
		{"DELETE FROM event_ingest WHERE received_at<=? ORDER BY received_at LIMIT ?", now.AddDate(0, 0, -int(s.Settings.Int("diagnostic_retention_days")))},
		{"DELETE FROM idempotency_record WHERE expires_at<=? ORDER BY expires_at LIMIT ?", now},
		{"DELETE FROM admin_audit WHERE created_at<=? ORDER BY created_at LIMIT ?", now.AddDate(0, 0, -int(s.Settings.Int("admin_audit_retention_days")))},
	} {
		if _, err = db.ExecContext(ctx, q.query, q.cutoff, limit); err != nil {
			slog.Warn("local data retention sweep failed")
		}
	}
}
