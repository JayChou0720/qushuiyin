package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"qushuiyin/service/internal/auth"
)

type ticketBudget struct {
	expires        time.Time
	used, reserved int64
}

func (s *Mock) remainingPreviewBytes(key string, limit int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.byteBudgets[key]; b != nil {
		return max(0, limit-b.used-b.reserved)
	}
	return limit
}

func (s *Mock) reservePreviewBytes(ctx context.Context, user, key string, expires time.Time, n, limit int64) (func(int64), error) {
	_, settle, err := s.reserveMediaBytes(ctx, user, key, expires, n, limit, false)
	return settle, err
}

// Reserve from the ticket first, then atomically cap against both daily quotas.
// Video previews can serve a shorter range when any byte allowance runs low.
func (s *Mock) reserveMediaBytes(ctx context.Context, user, key string, expires time.Time, n, limit int64, partial bool) (int64, func(int64), error) {
	s.mu.Lock()
	if s.byteBudgets == nil {
		s.byteBudgets = map[string]*ticketBudget{}
	}
	now := time.Now()
	for k, b := range s.byteBudgets {
		if !now.Before(b.expires) && b.reserved == 0 {
			delete(s.byteBudgets, k)
		}
	}
	b := s.byteBudgets[key]
	if b == nil {
		if int64(len(s.byteBudgets)) >= s.settings().Int("memory_media_ticket_max_entries") {
			s.mu.Unlock()
			return 0, nil, errors.New("SERVICE_BUSY")
		}
		b = &ticketBudget{expires: expires}
		s.byteBudgets[key] = b
	}
	remaining := limit - b.used - b.reserved
	if partial {
		n = min(n, remaining)
	}
	if n <= 0 || n > remaining || !now.Before(expires) {
		s.mu.Unlock()
		return 0, nil, errors.New("RATE_LIMITED")
	}
	b.reserved += n
	s.mu.Unlock()

	size, settle, err := s.reserveEgress(ctx, user, n, partial)
	s.mu.Lock()
	b.reserved -= n - size
	s.mu.Unlock()
	if err != nil {
		return 0, nil, err
	}
	var once sync.Once
	return size, func(actual int64) {
		once.Do(func() {
			actual = max(0, min(actual, size))
			s.mu.Lock()
			b.reserved -= size
			b.used += actual
			s.mu.Unlock()
			settle(actual)
		})
	}, nil
}

func (s *Mock) acquireMedia(user, ticket string) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := []string{"global", "user:" + user, "ticket:" + ticket}
	limits := []int64{s.settings().Int("global_media_connections"), s.settings().Int("user_media_connections"), s.settings().Int("ticket_connections")}
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
func (s *Mock) reserveEgress(ctx context.Context, user string, size int64, partial bool) (int64, func(int64), error) {
	day := time.Now().In(shanghaiDay).Format("2006-01-02")
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	subjects := []string{"global", user}
	limits := []int64{s.settings().Int("global_media_egress_per_day_bytes"), s.settings().Int("user_media_egress_per_day_bytes")}
	for i, subject := range subjects {
		_, err = tx.ExecContext(ctx, "INSERT INTO usage_counter(id,subject_key,stat_date,kind,value) VALUES (?,?,?,'media_egress',0) ON DUPLICATE KEY UPDATE value=value", auth.ID(), subject, day)
		if err != nil {
			return 0, nil, err
		}
		var current int64
		if err = tx.QueryRowContext(ctx, "SELECT value FROM usage_counter WHERE subject_key=? AND stat_date=? AND kind='media_egress' FOR UPDATE", subject, day).Scan(&current); err != nil {
			return 0, nil, err
		}
		remaining := limits[i] - current
		if partial {
			size = min(size, remaining)
		}
		if size <= 0 || size > remaining {
			return 0, nil, errors.New("media egress quota exceeded")
		}
	}
	for _, subject := range subjects {
		_, err = tx.ExecContext(ctx, "UPDATE usage_counter SET value=value+? WHERE subject_key=? AND stat_date=? AND kind='media_egress'", size, subject, day)
		if err != nil {
			return 0, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return size, func(actual int64) {
		settle, cancel := context.WithTimeout(context.Background(), s.settings().Seconds("database_operation_timeout_seconds"))
		defer cancel()
		// Failure leaves a conservative full reservation; it cannot overspend quota.
		_, _ = s.Sessions.DB.ExecContext(settle, "UPDATE usage_counter SET value=GREATEST(0,value-?) WHERE subject_key IN (?,?) AND stat_date=? AND kind='media_egress'", size-actual, "global", user, day)
	}, nil
}

type countedMediaResponse struct {
	http.ResponseWriter
	sent        int64
	status      int
	writeFailed bool
}

func (w *countedMediaResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *countedMediaResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, e := w.ResponseWriter.Write(p)
	w.sent += int64(n)
	w.writeFailed = w.writeFailed || e != nil || n != len(p)
	return n, e
}
func (w *countedMediaResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
