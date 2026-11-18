package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"qushuiyin/service/internal/auth"
)

func (s *Mock) acquireMedia(user, ticket string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := []string{"global", "user:" + user, "ticket:" + ticket}
	limits := []int64{s.Settings.Int("global_media_connections"), s.Settings.Int("user_media_connections"), s.Settings.Int("ticket_connections")}
	for i, key := range keys {
		if s.mediaConnections[key] >= limits[i] {
			return nil, false
		}
	}
	for _, key := range keys {
		s.mediaConnections[key]++
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, key := range keys {
			s.mediaConnections[key]--
			if s.mediaConnections[key] == 0 {
				delete(s.mediaConnections, key)
			}
		}
	}, true
}
func (s *Mock) reserveEgress(ctx context.Context, user string, size int64) (func(int64), error) {
	day := time.Now().In(shanghaiDay).Format("2006-01-02")
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	subjects := []string{"global", user}
	limits := []int64{s.Settings.Int("global_media_egress_per_day_bytes"), s.Settings.Int("user_media_egress_per_day_bytes")}
	for i, subject := range subjects {
		_, err = tx.ExecContext(ctx, "INSERT INTO usage_counter(id,subject_key,stat_date,kind,value) VALUES (?,?,?,'media_egress',0) ON DUPLICATE KEY UPDATE value=value", auth.ID(), subject, day)
		if err != nil {
			return nil, err
		}
		var current int64
		if err = tx.QueryRowContext(ctx, "SELECT value FROM usage_counter WHERE subject_key=? AND stat_date=? AND kind='media_egress' FOR UPDATE", subject, day).Scan(&current); err != nil {
			return nil, err
		}
		if size > limits[i]-current {
			return nil, errors.New("media egress quota exceeded")
		}
		_, err = tx.ExecContext(ctx, "UPDATE usage_counter SET value=value+? WHERE subject_key=? AND stat_date=? AND kind='media_egress'", size, subject, day)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return func(actual int64) {
		settle, cancel := context.WithTimeout(context.Background(), s.Settings.Seconds("parse_job_timeout_seconds"))
		defer cancel()
		// Failure leaves a conservative full reservation; it cannot overspend quota.
		_, _ = s.Sessions.DB.ExecContext(settle, "UPDATE usage_counter SET value=GREATEST(0,value-?) WHERE subject_key IN (?,?) AND stat_date=? AND kind='media_egress'", size-actual, "global", user, day)
	}, nil
}

type countedMediaResponse struct {
	http.ResponseWriter
	sent int64
}

func (w *countedMediaResponse) Write(p []byte) (int, error) {
	n, e := w.ResponseWriter.Write(p)
	w.sent += int64(n)
	return n, e
}
func (w *countedMediaResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
