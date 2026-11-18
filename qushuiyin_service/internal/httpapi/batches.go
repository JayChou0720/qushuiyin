package httpapi

import (
	"bytes"
	"crypto/hmac"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"qushuiyin/service/internal/auth"
	fixture "qushuiyin/service/testdata/media"
)

type batch struct {
	ID, User, ResultID, ClientActionID, Status string
	ExpiresAt                                  time.Time
	Items                                      []item
}
type item struct {
	ID, ObjectID string
	Asset        asset
}
type batchRequest struct {
	ResultID               string
	AssetIDs               []string
	ClientActionID, FlowID string
}

func (s *Mock) createBatch(w http.ResponseWriter, r *http.Request, c credential) {
	var req batchRequest
	if !s.read(w, r, &req) {
		return
	}
	if len(req.AssetIDs) == 0 || int64(len(req.AssetIDs)) > s.Settings.Int("max_image_batch_count") || req.ClientActionID == "" || len(req.ClientActionID) > 64 || req.FlowID == "" || len(req.FlowID) > 64 {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	key, replay, proceed := s.replay(w, r, c, "prepare", req, now)
	if !proceed {
		return
	}
	if replay.ID != "" {
		b := s.batches[replay.ID]
		if b == nil {
			fail(w, 410, "BATCH_EXPIRED")
			return
		}
		s.batchAccepted(w, b)
		return
	}
	res := s.results[req.ResultID]
	if res == nil || res.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	required := s.Settings.Seconds("batch_prepare_timeout_seconds") + s.Settings.Seconds("client_download_timeout_seconds") + s.Settings.Seconds("ticket_safety_margin_seconds")
	if res.ExpiresAt.Sub(now) < required {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	if int64(len(s.batches)) >= s.Settings.Int("memory_parse_cache_max_entries") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	active := 0
	for _, b := range s.batches {
		if b.User == c.User && b.ClientActionID == req.ClientActionID {
			fail(w, 409, "IDEMPOTENCY_CONFLICT")
			return
		}
		if b.User == c.User && b.Status == "PREPARING" {
			active++
		}
	}
	if int64(active) >= s.Settings.Int("user_active_save_batches") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	pool := map[string]asset{}
	for _, a := range res.Assets {
		pool[a.ID] = a
	}
	if res.Cover != nil {
		pool[res.Cover.ID] = *res.Cover
	}
	b := &batch{ID: auth.ID(), User: c.User, ResultID: res.ID, ClientActionID: req.ClientActionID, Status: "PREPARING", ExpiresAt: minTime(res.ExpiresAt, now.Add(s.Settings.Seconds("batch_ttl_seconds")))}
	for _, id := range req.AssetIDs {
		a, ok := pool[id]
		if !ok {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		delete(pool, id)
		if len(req.AssetIDs) > 1 && (a.Role != "content" || a.Kind != "image") {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		b.Items = append(b.Items, item{auth.ID(), auth.ID(), a})
	}
	sort.Slice(b.Items, func(i, j int) bool { return b.Items[i].Asset.Order < b.Items[j].Asset.Order })
	s.batches[b.ID] = b
	replay.ID = b.ID
	s.keys[key] = replay
	s.batchAccepted(w, b)
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (s *Mock) batchAccepted(w http.ResponseWriter, b *batch) {
	ok(w, 202, dto{"batchId": b.ID, "status": b.Status, "pollAfterMs": s.Settings.Int("poll_interval_ms"), "expiresAt": b.ExpiresAt})
}
func (s *Mock) ownedBatch(w http.ResponseWriter, r *http.Request, c credential) *batch {
	b := s.batches[r.PathValue("batchId")]
	if b == nil || b.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return nil
	}
	if !time.Now().Before(b.ExpiresAt) {
		fail(w, 410, "BATCH_EXPIRED")
		return nil
	}
	return b
}
func (s *Mock) batchStatus(w http.ResponseWriter, r *http.Request, c credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	if r.Method == "DELETE" {
		b.Status = "CANCELLED"
	} else if b.Status == "PREPARING" {
		b.Status = "READY"
	}
	items := make([]dto, 0, len(b.Items))
	for _, x := range b.Items {
		var mode, oid, kind, size any
		if b.Status == "READY" {
			mime, raw := fixture.File(x.Asset.Kind)
			mode = "relay"
			oid = x.ObjectID
			kind = mime
			size = len(raw)
		}
		items = append(items, dto{"itemId": x.ID, "assetId": x.Asset.ID, "order": x.Asset.Order, "prepareStatus": b.Status, "deliveryMode": mode, "copyLinkAvailable": false, "attempt": 1, "mediaObjectId": oid, "mimeType": kind, "sizeBytes": size, "converted": false, "error": nil})
	}
	ok(w, 200, dto{"batchId": b.ID, "status": b.Status, "expiresAt": b.ExpiresAt, "items": items})
}
func (s *Mock) signature(bid, oid, digest, expire string) string {
	return auth.Digest(s.TicketKey, "mock-download:"+bid+":"+oid+":"+digest+":"+expire)
}
func (s *Mock) tickets(w http.ResponseWriter, r *http.Request, c credential) {
	var req struct{ ItemIDs []string }
	if !s.read(w, r, &req) {
		return
	}
	if len(req.ItemIDs) == 0 || int64(len(req.ItemIDs)) > s.Settings.Int("max_image_batch_count") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	if b.Status != "READY" {
		fail(w, 409, "FILE_NOT_READY")
		return
	}
	if !s.minute("ticket:"+c.User, s.Settings.Int("user_download_url_requests_per_minute")) {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	expires := minTime(b.ExpiresAt, time.Now().Add(s.Settings.Seconds("media_ticket_ttl_seconds"))).UTC().Truncate(time.Second)
	epoch := strconv.FormatInt(expires.Unix(), 10)
	if time.Until(expires) < s.Settings.Seconds("client_download_timeout_seconds")+s.Settings.Seconds("ticket_safety_margin_seconds") {
		fail(w, 410, "BATCH_EXPIRED")
		return
	}
	pool := map[string]item{}
	for _, x := range b.Items {
		pool[x.ID] = x
	}
	out := make([]dto, 0, len(req.ItemIDs))
	for _, id := range req.ItemIDs {
		x, ok := pool[id]
		if !ok {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		delete(pool, id)
		q := url.Values{"b": {b.ID}, "s": {c.Digest}, "e": {epoch}, "t": {s.signature(b.ID, x.ObjectID, c.Digest, epoch)}}
		out = append(out, dto{"itemId": id, "deliveryMode": "relay", "url": s.Origin + "/media/download/" + x.ObjectID + "?" + q.Encode(), "expiresAt": expires})
	}
	ok(w, 200, dto{"tickets": out})
}
func (s *Mock) download(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bid, session, epoch, sig := q.Get("b"), q.Get("s"), q.Get("e"), q.Get("t")
	oid := r.PathValue("mediaObjectId")
	n, e := strconv.ParseInt(epoch, 10, 64)
	if e != nil || time.Now().Unix() >= n || len(bid) > 64 || len(session) != 64 || len(oid) > 64 || !hmac.Equal([]byte(sig), []byte(s.signature(bid, oid, session, epoch))) {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	uid, err := s.Sessions.User(r.Context(), session)
	if err != nil {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	s.mu.Lock()
	kind := ""
	b := s.batches[bid]
	if b != nil && b.User == uid && b.Status == "READY" && time.Now().Before(b.ExpiresAt) {
		for _, i := range b.Items {
			if i.ObjectID == oid {
				kind = i.Asset.Kind
				break
			}
		}
	}
	s.mu.Unlock()
	if kind == "" {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	mime, data := fixture.File(kind)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "sample", time.Time{}, bytes.NewReader(data))
}
