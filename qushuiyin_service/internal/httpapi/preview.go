package httpapi

import (
	"bytes"
	"crypto/hmac"
	"net/http"
	"net/url"
	"strings"
	"time"

	"qushuiyin/service/internal/auth"
	fixture "qushuiyin/service/testdata/media"
)

type preview struct {
	user, session, resultID, kind string
	expires                       time.Time
	remaining                     int64
}

func (s *Mock) previewTickets(w http.ResponseWriter, r *http.Request, c credential) {
	// Until the separate bounded preview pipeline is ready, live results must
	// never return the synthetic media used by local Mock previews.
	if s.Relay != nil {
		fail(w, 503, "DIRECT_DOWNLOAD_UNAVAILABLE")
		return
	}
	var req struct{ AssetIDs []string }
	if !s.read(w, r, &req) {
		return
	}
	if len(req.AssetIDs) == 0 || int64(len(req.AssetIDs)) > s.Settings.Int("max_image_batch_count") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	now := time.Now().UTC()
	if s.Persistent {
		res, err := s.loadPersistentResult(r.Context(), c.User, r.PathValue("resultId"))
		if err != nil {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		s.mu.Lock()
		s.results[res.ID] = res.result
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.results[r.PathValue("resultId")]
	if res == nil || res.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if !now.Before(res.ExpiresAt) {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	pool := make(map[string]asset, len(res.Assets)+1)
	for _, a := range res.Assets {
		pool[a.ID] = a
	}
	if res.Cover != nil {
		pool[res.Cover.ID] = *res.Cover
	}
	chosen := make([]asset, 0, len(req.AssetIDs))
	for _, id := range req.AssetIDs {
		a, exists := pool[id]
		if !exists || (a.Kind == "video" && len(req.AssetIDs) != 1) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		delete(pool, id)
		chosen = append(chosen, a)
	}
	if !s.minute("ticket:"+c.User, s.Settings.Int("user_download_url_requests_per_minute")) {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	if int64(len(s.previews)+len(chosen)) > s.Settings.Int("memory_parse_cache_max_entries") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	expires := minTime(res.ExpiresAt, now.Add(s.Settings.Seconds("media_ticket_ttl_seconds")))
	entries := make([]dto, 0, len(chosen))
	for _, a := range chosen {
		id := auth.ID()
		limit := s.Settings.Int("preview_max_bytes")
		_, data := fixture.File(a.Kind)
		if int64(len(data)) > limit {
			entries = append(entries, dto{"assetId": a.ID, "mode": "relay", "url": nil, "expiresAt": nil, "reasonCode": "MEDIA_TOO_LARGE"})
			continue
		}
		s.previews[id] = &preview{c.User, c.Digest, res.ID, a.Kind, expires, limit}
		var path string
		if a.Kind == "video" {
			path = "preview"
		} else {
			path = "thumbnail"
		}
		q := url.Values{"t": {s.previewSignature(path, id)}}
		entries = append(entries, dto{"assetId": a.ID, "mode": "relay", "url": s.Origin + "/media/" + path + "/" + id + "?" + q.Encode(), "expiresAt": expires})
	}
	ok(w, 200, dto{"previews": entries})
}

func (s *Mock) previewSignature(path, id string) string {
	return auth.Digest(s.TicketKey, "mock-preview:"+path+":"+id)
}

func (s *Mock) previewMedia(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("previewId")
		signature := r.URL.Query().Get("t")
		if len(id) > 64 || !hmac.Equal([]byte(signature), []byte(s.previewSignature(path, id))) {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		s.mu.Lock()
		item := s.previews[id]
		if item == nil || !time.Now().Before(item.expires) || s.results[item.resultID] == nil {
			s.mu.Unlock()
			fail(w, 410, "RESULT_EXPIRED")
			return
		}
		user, session, kind := item.user, item.session, item.kind
		s.mu.Unlock()
		if (path == "preview") != (kind == "video") {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		uid, err := s.Sessions.User(r.Context(), session)
		if err != nil || uid != user {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		ranges := r.Header.Values("Range")
		if len(ranges) > 1 || (len(ranges) == 1 && strings.Contains(ranges[0], ",")) {
			fail(w, 416, "BAD_REQUEST")
			return
		}
		mime, data := fixture.File(kind)
		if r.Method == http.MethodGet {
			s.mu.Lock()
			// Reserve at most one full fixture per request, even for a short range.
			available := s.previews[id] == item && time.Now().Before(item.expires) && item.remaining >= int64(len(data))
			if available {
				item.remaining -= int64(len(data))
			}
			s.mu.Unlock()
			if !available {
				fail(w, 429, "RATE_LIMITED")
				return
			}
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "sample", time.Time{}, bytes.NewReader(data))
	}
}
