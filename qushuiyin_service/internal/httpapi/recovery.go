package httpapi

import (
	"net/http"
	"time"

	"qushuiyin/service/internal/auth"
)

func (b *batch) aggregate() {
	counts := map[string]int{}
	for _, x := range b.Items {
		counts[x.Status]++
	}
	switch {
	case b.Cancelled || counts["CANCELLED"] == len(b.Items):
		b.Status = "CANCELLED"
	case counts["PREPARING"]+counts["PENDING"] > 0:
		b.Status = "PREPARING"
	case counts["READY"] == len(b.Items):
		b.Status = "READY"
	case counts["READY"] > 0:
		b.Status = "PARTIAL_READY"
	case counts["FAILED"] > 0:
		b.Status = "FAILED"
	default:
		b.Status = "EXPIRED"
	}
}
func (s *Mock) downloadBudget() time.Duration {
	return s.Settings.Seconds("client_download_timeout_seconds") + s.Settings.Seconds("ticket_safety_margin_seconds")
}
func (s *Mock) refreshBatch(b *batch, now time.Time) {
	for i := range b.Items {
		x := &b.Items[i]
		if x.Status == "READY" && x.AvailableUntil.Sub(now) < s.downloadBudget() {
			x.Status = "EXPIRED"
			x.Failure = "MEDIA_SOURCE_EXPIRED"
		}
	}
	b.aggregate()
}

func (s *Mock) prepare(b *batch, x *item, now time.Time) {
	res := s.results[b.ResultID]
	if res == nil || !now.Before(res.ExpiresAt) {
		x.Status, x.Failure = "EXPIRED", "MEDIA_SOURCE_EXPIRED"
		return
	}
	if !now.Before(x.Deadline) {
		x.Status, x.Failure = "FAILED", "PREPARE_TIMEOUT"
		return
	}
	scenario := ""
	if res.Value.Platform == "douyin" && res.Value.ContentID != nil && x.Asset.Kind == "video" {
		scenario = *res.Value.ContentID
	}
	x.Failure = ""
	if scenario == "10004" && x.Attempt == 1 {
		x.Status, x.Failure = "FAILED", "PREPARE_TIMEOUT"
		return
	}
	x.ObjectID = auth.ID()
	if scenario == "10006" && x.Fallback {
		x.Status, x.Mode, x.Failure, x.CopyAvailable = "FAILED", "", "DIRECT_DOWNLOAD_UNAVAILABLE", true
		x.AvailableUntil = minTime(b.ExpiresAt, now.Add(s.Settings.Seconds("direct_url_assumed_ttl_seconds")))
		return
	}
	x.Status, x.Mode = "READY", "relay"
	ttl := s.Settings.Seconds("media_cache_ttl_seconds")
	if (scenario == "10005" || scenario == "10006") && !x.Fallback {
		x.Mode = "direct"
		ttl = s.Settings.Seconds("direct_url_assumed_ttl_seconds")
	}
	x.AvailableUntil = minTime(b.ExpiresAt, now.Add(ttl))
}

func (s *Mock) canPrepare(w http.ResponseWriter, r *http.Request, b *batch, now time.Time) bool {
	expires, err := s.batchResultExpiry(r.Context(), b.User, b.ResultID)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return false
	}
	budget := s.Settings.Seconds("batch_prepare_timeout_seconds") + s.downloadBudget()
	if expires.Sub(now) < budget {
		fail(w, 410, "RESULT_EXPIRED")
		return false
	}
	if b.ExpiresAt.Sub(now) < budget {
		fail(w, 410, "BATCH_EXPIRED")
		return false
	}
	if b.Status != "PREPARING" {
		n, _, err := s.batchCapacity(r.Context(), b.User, "", now)
		if err != nil || n >= s.Settings.Int("user_active_save_batches") {
			fail(w, 503, "SERVICE_BUSY")
			return false
		}
	}
	return true
}
func (s *Mock) requeue(b *batch, x *item, now time.Time) {
	x.Attempt++
	x.Status, x.Mode, x.ObjectID, x.Failure, x.CopyAvailable = "PREPARING", "", "", "", false
	x.DirectURL, x.FileID, x.MIME, x.Hash = "", "", "", ""
	x.Converted = false
	x.Size = 0
	x.Deadline = minTime(b.ExpiresAt, now.Add(s.Settings.Seconds("batch_prepare_timeout_seconds")))
}
func (s *Mock) retryBatch(w http.ResponseWriter, r *http.Request, c credential) {
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
	now := time.Now().UTC()
	s.pruneMockBatches(now)
	key, replay, proceed := s.replayWithKeys(w, r, c, "batch-retry", []any{b.ID, req}, now, state.keys)
	if !proceed {
		return
	}
	if replay.ID != "" {
		s.batchAccepted(w, b)
		return
	}
	if b.Cancelled {
		fail(w, 409, "FILE_NOT_READY")
		return
	}
	s.refreshBatch(b, now)
	pool := map[string]*item{}
	for i := range b.Items {
		pool[b.Items[i].ID] = &b.Items[i]
	}
	chosen := make([]*item, 0, len(req.ItemIDs))
	seen := map[string]bool{}
	for _, id := range req.ItemIDs {
		if seen[id] {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		seen[id] = true
		x := pool[id]
		if x == nil {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		retryable := x.Status == "EXPIRED" || x.Status == "CANCELLED" || (x.Status == "FAILED" && (x.Failure == "PREPARE_TIMEOUT" || (x.CopyAvailable && !now.Before(x.AvailableUntil))))
		if !retryable {
			fail(w, 409, "FILE_NOT_READY")
			return
		}
		if x.Attempt >= s.Settings.Int("media_prepare_max_attempts_per_item") {
			fail(w, 409, "RETRY_LIMIT_REACHED")
			return
		}
		chosen = append(chosen, x)
	}
	if !s.canPrepare(w, r, b, now) {
		return
	}
	for _, x := range chosen {
		s.requeue(b, x, now)
	}
	b.aggregate()
	replay.ID = b.ID
	state.keys[key] = replay
	s.batchAccepted(w, b)
}
func (s *Mock) fallbackItem(w http.ResponseWriter, r *http.Request, c credential) {
	var req struct{ Reason string }
	if !s.read(w, r, &req) {
		return
	}
	if req.Reason != "DIRECT_DOWNLOAD_REJECTED" && req.Reason != "DIRECT_CONTENT_INVALID" {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	state, unlock := s.batchState(r)
	defer unlock()
	b := state.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	x := b.findItem(r.PathValue("itemId"))
	if x == nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	now := time.Now().UTC()
	s.pruneMockBatches(now)
	key, replay, proceed := s.replayWithKeys(w, r, c, "batch-fallback", []any{b.ID, x.ID, req}, now, state.keys)
	if !proceed {
		return
	}
	if replay.ID != "" {
		s.batchAccepted(w, b)
		return
	}
	s.refreshBatch(b, now)
	if b.Cancelled || x.Mode != "direct" || x.Fallback || (x.Status != "READY" && x.Status != "EXPIRED") {
		fail(w, 409, "FALLBACK_NOT_ALLOWED")
		return
	}
	if !s.canPrepare(w, r, b, now) {
		return
	}
	x.Fallback = true
	x.FallbackPending = true
	s.requeue(b, x, now)
	b.aggregate()
	replay.ID = b.ID
	state.keys[key] = replay
	s.batchAccepted(w, b)
}
func (b *batch) findItem(id string) *item {
	for i := range b.Items {
		if b.Items[i].ID == id {
			return &b.Items[i]
		}
	}
	return nil
}
func (s *Mock) copyLink(w http.ResponseWriter, r *http.Request, c credential) {
	state, unlock := s.batchState(r)
	defer unlock()
	b := state.ownedBatch(w, r, c)
	if b == nil {
		return
	}
	x := b.findItem(r.PathValue("itemId"))
	if x == nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if b.Cancelled || x.Status != "FAILED" || !x.CopyAvailable {
		fail(w, 409, "FILE_NOT_READY")
		return
	}
	expires := x.AvailableUntil.UTC().Truncate(time.Second)
	if !time.Now().Before(expires) {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	if !s.batchURLRate(c.User) || s.Relay == nil && !s.batchCopyRate(c.User) {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	address := s.mediaURL("copy", b.ID, x.ObjectID, c.Digest, expires)
	if s.Relay != nil {
		if x.DirectURL == "" {
			fail(w, 410, "MEDIA_SOURCE_EXPIRED")
			return
		}
		address = x.DirectURL
		x.CopyPending++
	}
	ok(w, 200, dto{"url": address, "expiresAt": expires})
}
