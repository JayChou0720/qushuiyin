package httpapi

import (
	"bytes"
	"crypto/hmac"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"qushuiyin/service/internal/auth"
	fixture "qushuiyin/service/testdata/media"
)

type batch struct {
	ReparseAttempts, RefreshAttempt            int64
	SourceOverrides                            map[string]sourceSnapshot
	OutputVersion                              string
	Version                                    uint64
	ID, User, ResultID, ClientActionID, Status string
	FlowID                                     string
	ExpiresAt                                  time.Time
	Items                                      []item
	Cancelled                                  bool
}
type item struct {
	DirectURL                                            string `json:"-"`
	DirectHost                                           string
	DirectIssuedPending, CopyPending, RelayIssuedPending int64 `json:"-"`
	FallbackPending                                      bool  `json:"-"`
	FileID, MIME, Hash                                   string
	Size                                                 int64
	Converted                                            bool
	ID, ObjectID, Status, Mode, Failure                  string
	Asset                                                asset
	Attempt                                              int64
	Deadline, AvailableUntil                             time.Time
	Fallback, CopyAvailable                              bool
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
	state, unlock := s.batchState(r)
	defer unlock()
	s.pruneMockBatches(now)
	key, replay, proceed := s.replayWithKeys(w, r, c, "prepare", req, now, state.keys)
	if !proceed {
		return
	}
	if replay.ID != "" {
		b := state.batches[replay.ID]
		if b == nil || !now.Before(b.ExpiresAt) {
			fail(w, 410, "BATCH_EXPIRED")
			return
		}
		s.batchAccepted(w, b)
		return
	}
	res, err := s.batchResult(r.Context(), c.User, req.ResultID)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	if res == nil || res.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	required := s.Settings.Seconds("batch_prepare_timeout_seconds") + s.Settings.Seconds("client_download_timeout_seconds") + s.Settings.Seconds("ticket_safety_margin_seconds")
	if res.ExpiresAt.Sub(now) < required {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	if int64(len(state.batches)) >= s.Settings.Int("memory_parse_cache_max_entries") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	active, duplicate, err := s.batchCapacity(r.Context(), c.User, req.ClientActionID, now)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	if duplicate {
		fail(w, 409, "IDEMPOTENCY_CONFLICT")
		return
	}
	if active >= s.Settings.Int("user_active_save_batches") {
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
	b := &batch{ID: auth.ID(), User: c.User, ResultID: res.ID, ClientActionID: req.ClientActionID, FlowID: req.FlowID, Status: "PREPARING", ExpiresAt: minTime(res.ExpiresAt, now.Add(s.Settings.Seconds("batch_ttl_seconds")))}
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
		b.Items = append(b.Items, item{ID: auth.ID(), Asset: a, Status: "PREPARING", Attempt: 1, Deadline: minTime(b.ExpiresAt, now.Add(s.Settings.Seconds("batch_prepare_timeout_seconds")))})
	}
	sort.Slice(b.Items, func(i, j int) bool { return b.Items[i].Asset.Order < b.Items[j].Asset.Order })
	state.batches[b.ID] = b
	replay.ID = b.ID
	state.keys[key] = replay
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
func (state batchRequestState) ownedBatch(w http.ResponseWriter, r *http.Request, c credential) *batch {
	b := state.batches[r.PathValue("batchId")]
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
	state, unlock := s.batchState(r)
	defer unlock()
	b := state.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	if r.Method == "DELETE" {
		b.Cancelled = true
		for i := range b.Items {
			if b.Items[i].Status == "PREPARING" || b.Items[i].Status == "PENDING" {
				b.Items[i].Status = "CANCELLED"
			}
		}
	} else if !b.Cancelled {
		now := time.Now().UTC()
		for i := range b.Items {
			if !s.Persistent && b.Items[i].Status == "PREPARING" {
				s.prepare(b, &b.Items[i], now)
			}
		}
		s.refreshBatch(b, now)
	}
	b.aggregate()
	items := make([]dto, 0, len(b.Items))
	for _, x := range b.Items {
		var mode, oid, kind, size, failure any
		if x.Mode != "" {
			mode = x.Mode
		}
		if x.Status == "READY" {
			mime, raw := fixture.File(x.Asset.Kind)
			if x.Mode == "relay" {
				oid = x.ObjectID
			}
			kind = mime
			size = len(raw)
			if s.Relay != nil {
				kind = x.MIME
				size = x.Size
			}
		}
		if x.Failure != "" {
			failure = issue(x.Failure)
		}
		items = append(items, dto{"itemId": x.ID, "assetId": x.Asset.ID, "order": x.Asset.Order, "prepareStatus": x.Status, "deliveryMode": mode, "copyLinkAvailable": x.CopyAvailable && !b.Cancelled, "attempt": x.Attempt, "mediaObjectId": oid, "mimeType": kind, "sizeBytes": size, "converted": x.Status == "READY" && x.Mode == "relay" && x.Converted, "error": failure})
	}
	ok(w, 200, dto{"batchId": b.ID, "status": b.Status, "expiresAt": b.ExpiresAt, "items": items})
}
func (s *Mock) signature(mode, bid, oid, digest, expire, ticket string) string {
	return auth.Digest(s.TicketKey, "download:v1:"+mode+":"+bid+":"+oid+":"+digest+":"+expire+":"+ticket)
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
	state, unlock := s.batchState(r)
	defer unlock()
	b := state.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	if b.Cancelled {
		fail(w, 409, "FILE_NOT_READY")
		return
	}
	if !s.batchURLRate(c.User) {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	now := time.Now().UTC()
	pool := map[string]*item{}
	for i := range b.Items {
		pool[b.Items[i].ID] = &b.Items[i]
	}
	out := make([]dto, 0, len(req.ItemIDs))
	for _, id := range req.ItemIDs {
		x, ok := pool[id]
		if !ok {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		delete(pool, id)
		expireItem := func() {
			x.expire()
			b.aggregate()
			fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		}
		if x.Status == "READY" && x.AvailableUntil.Sub(now) < s.downloadBudget() {
			expireItem()
			return
		}
		if x.Status == "EXPIRED" {
			fail(w, 410, "MEDIA_SOURCE_EXPIRED")
			return
		}
		if x.Status != "READY" {
			fail(w, 409, "FILE_NOT_READY")
			return
		}
		expires := x.AvailableUntil
		if s.Relay != nil && x.Mode == "relay" {
			var authorizationExpiry time.Time
			err := s.Sessions.DB.QueryRowContext(r.Context(), `SELECT LEAST(o.expires_at,f.expires_at,b.expires_at) FROM media_object o JOIN media_file f ON f.id=o.file_id JOIN save_batch_item i ON i.media_object_id=o.id JOIN save_batch b ON b.id=i.batch_id WHERE o.id=? AND o.user_id=? AND i.user_id=? AND b.user_id=? AND b.id=? AND o.status='ready' AND f.status='ready' AND i.prepare_status='READY' AND b.cancel_requested=FALSE AND o.expires_at>UTC_TIMESTAMP(3) AND f.expires_at>UTC_TIMESTAMP(3) AND b.expires_at>UTC_TIMESTAMP(3)`, x.ObjectID, c.User, c.User, c.User, b.ID).Scan(&authorizationExpiry)
			if errors.Is(err, sql.ErrNoRows) {
				expireItem()
				return
			}
			if err != nil {
				fail(w, 503, "SERVICE_BUSY")
				return
			}
			expires = minTime(expires, authorizationExpiry)
		}
		if x.Mode == "relay" {
			expires = minTime(expires, now.Add(s.Settings.Seconds("media_ticket_ttl_seconds")))
		}
		expires = expires.UTC().Truncate(time.Second)
		if expires.Sub(now) < s.downloadBudget() {
			expireItem()
			return
		}
		address := s.mediaURL(x.Mode, b.ID, x.ObjectID, c.Digest, expires)
		if s.Relay != nil && x.Mode == "direct" {
			var state string
			err := s.Sessions.DB.QueryRowContext(r.Context(), "SELECT status FROM download_domain WHERE host=?", x.DirectHost).Scan(&state)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				fail(w, 503, "SERVICE_BUSY")
				return
			}
			if x.DirectURL == "" || state != "registered" {
				expireItem()
				return
			}
			address = x.DirectURL
			x.DirectIssuedPending++
		}
		if s.Relay != nil && x.Mode == "relay" {
			x.RelayIssuedPending++
		}
		out = append(out, dto{"itemId": id, "deliveryMode": x.Mode, "url": address, "expiresAt": expires})
	}
	ok(w, 200, dto{"tickets": out})
}
func (s *Mock) mediaURL(mode, bid, oid, digest string, expires time.Time) string {
	path := "download"
	if mode != "relay" {
		path = "mock-source/" + mode
	}
	epoch := strconv.FormatInt(expires.Unix(), 10)
	ticket := auth.ID()
	q := url.Values{"b": {bid}, "s": {digest}, "e": {epoch}, "v": {"1"}, "i": {ticket}, "t": {s.signature(mode, bid, oid, digest, epoch, ticket)}}
	return s.Origin + "/media/" + path + "/" + oid + "?" + q.Encode()
}
func (s *Mock) download(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Relay != nil && mode != "relay" {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		q := r.URL.Query()
		bid, session, epoch, sig := q.Get("b"), q.Get("s"), q.Get("e"), q.Get("t")
		oid := r.PathValue("mediaObjectId")
		n, e := strconv.ParseInt(epoch, 10, 64)
		if e != nil || time.Now().Unix() >= n || len(bid) > 64 || len(session) != 64 || len(oid) > 64 || q.Get("v") != "1" || len(q.Get("i")) == 0 || len(q.Get("i")) > 64 || !hmac.Equal([]byte(sig), []byte(s.signature(mode, bid, oid, session, epoch, q.Get("i")))) {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		uid, err := s.Sessions.User(r.Context(), session)
		if err != nil {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		if s.Relay != nil && mode == "relay" {
			s.relayDownload(w, r, uid, bid, oid)
			return
		}
		var b *batch
		if s.Persistent {
			b, err = s.loadBatch(r.Context(), uid, bid, false)
			if err != nil {
				fail(w, 503, "SERVICE_BUSY")
				return
			}
		} else {
			s.mu.Lock()
			b = s.batches[bid]
		}
		kind := ""
		if b != nil && b.User == uid && !b.Cancelled && time.Now().Before(b.ExpiresAt) {
			for _, i := range b.Items {
				valid := (i.Status == "READY" && mode == i.Mode) || (mode == "copy" && i.Status == "FAILED" && i.CopyAvailable)
				if i.ObjectID == oid && valid && time.Now().Before(i.AvailableUntil) {
					kind = i.Asset.Kind
					break
				}
			}
		}
		if !s.Persistent {
			s.mu.Unlock()
		}
		if kind == "" {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		// The direct fixtures simulate an upstream that refuses the mini-program download.
		if mode == "direct" {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		mime, data := fixture.File(kind)
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "sample", time.Time{}, bytes.NewReader(data))
	}
}
