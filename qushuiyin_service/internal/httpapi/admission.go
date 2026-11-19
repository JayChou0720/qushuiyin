package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

func (s *Mock) admitMinute(w http.ResponseWriter, key string, limit int64) bool {
	now := time.Now()
	s.mu.Lock()
	allowed := s.rateUntil(key, limit, now.Add(time.Minute))
	until := s.rates[key].Until
	s.mu.Unlock()
	if !allowed {
		if until.IsZero() {
			until = now.Add(time.Minute)
		}
		retry := max(1, int64((until.Sub(now)+time.Second-1)/time.Second))
		w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
		send(w, 429, "RATE_LIMITED", nil, dto{"retryAfterSeconds": retry})
	}
	return allowed
}

func (s *Mock) poll(next authed) authed {
	return func(w http.ResponseWriter, r *http.Request, c credential) {
		limit := s.settings().Int("user_poll_requests_per_minute")
		if limit <= 0 || s.admitMinute(w, "poll:"+c.User, limit) {
			next(w, r, c)
		}
	}
}

func (s *Mock) health(w http.ResponseWriter, r *http.Request) {
	if !peerIP(r).IsLoopback() {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.settings().Seconds("database_operation_timeout_seconds"))
	defer cancel()
	if s.Sessions.DB == nil || s.Sessions.DB.PingContext(ctx) != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, dto{})
}
