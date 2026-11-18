package httpapi

import (
	"context"
	"crypto/hmac"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/media"
)

func (s *Mock) livePreviewSignature(p *preview, id, purpose string) string {
	return auth.Digest(s.TicketKey, fmt.Sprintf("preview:%s:%s:%s:%s:%s:%s:%d:%d", id, p.assetID, p.resultID, p.user, p.session, purpose, p.version, p.expires.UnixMilli()))
}

func (s *Mock) livePreviewTickets(w http.ResponseWriter, r *http.Request, c credential) {
	var req struct{ AssetIDs []string }
	if !s.read(w, r, &req) {
		return
	}
	if len(req.AssetIDs) == 0 || int64(len(req.AssetIDs)) > s.Settings.Int("max_image_batch_count") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	res, err := s.loadPersistentResult(r.Context(), c.User, r.PathValue("resultId"))
	if err != nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	now := time.Now().UTC()
	if !now.Before(res.ExpiresAt) {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	seen := map[string]bool{}
	for _, id := range req.AssetIDs {
		a := sourceAsset(res.result, id)
		if a == nil || seen[id] || a.Kind == "video" && len(req.AssetIDs) != 1 {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		seen[id] = true
	}
	s.mu.Lock()
	allowed := s.minute("ticket:"+c.User, s.Settings.Int("user_download_url_requests_per_minute"))
	s.mu.Unlock()
	if !allowed {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	entries := make([]dto, 0, len(req.AssetIDs))
	for _, aid := range req.AssetIDs {
		a := sourceAsset(res.result, aid)
		entry := dto{"assetId": aid, "mode": "relay", "url": nil, "expiresAt": nil}
		policy, e := s.deliveryPolicy(r.Context(), res.result, asset{ID: aid, Kind: a.Kind, Role: a.Role, Order: a.Order})
		if e != nil || policy == "copy_only" {
			entry["reasonCode"] = "DIRECT_DOWNLOAD_UNAVAILABLE"
			entries = append(entries, entry)
			continue
		}
		expires := minTime(res.ExpiresAt, now.Add(s.Settings.Seconds("media_ticket_ttl_seconds")))
		if s.Settings.Bool("direct_preview_enabled") && policy == "direct_preferred" {
			d := s.Relay.Decide(r.Context(), res.Value.Platform, policy, *a, false)
			if d.Mode == "direct" {
				entry["mode"], entry["url"], entry["expiresAt"] = "direct", d.URL, minTime(expires, d.Expires)
				if e = s.recordPreviewIssued(r.Context(), res.Value.Platform, "direct_preview"); e != nil {
					entry["url"] = nil
					entry["expiresAt"] = nil
					entry["reasonCode"] = "SERVICE_BUSY"
				}
				entries = append(entries, entry)
				continue
			}
		}
		var version int64
		if e = s.Sessions.DB.QueryRowContext(r.Context(), "SELECT version FROM media_asset WHERE id=? AND user_id=? AND result_id=?", aid, c.User, res.ID).Scan(&version); e != nil {
			entry["reasonCode"] = "SERVICE_BUSY"
			entries = append(entries, entry)
			continue
		}
		id := auth.ID()
		p := &preview{user: c.User, session: c.Digest, resultID: res.ID, kind: a.Kind, assetID: aid, version: version, expires: expires}
		purpose := "thumbnail"
		if a.Kind == "video" {
			purpose = "preview"
		}
		q := url.Values{"i": {id}, "t": {s.livePreviewSignature(p, id, purpose)}}
		// The existing client changes only the path when opening a large image.
		// Give it a distinct image signature; thumbnail signatures cannot authorize it.
		if purpose == "thumbnail" {
			q.Set("it", s.livePreviewSignature(p, id, "image"))
		}
		s.mu.Lock()
		for key, old := range s.previews {
			if !now.Before(old.expires) {
				delete(s.previews, key)
			}
		}
		full := int64(len(s.previews)) >= s.Settings.Int("memory_parse_cache_max_entries")
		if !full {
			s.previews[id] = p
		}
		s.mu.Unlock()
		if full {
			entry["reasonCode"] = "SERVICE_BUSY"
		} else {
			entry["url"] = s.Origin + "/media/" + purpose + "/" + aid + "?" + q.Encode()
			entry["expiresAt"] = expires
		}
		entries = append(entries, entry)
	}
	ok(w, 200, dto{"previews": entries})
}

func (s *Mock) recordPreviewIssued(ctx context.Context, platform, outcome string) error {
	tx, e := s.Sessions.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = recordMediaMetric(ctx, tx, platform, "download_url_issued", "issued", outcome, 1); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Mock) livePreviewMedia(w http.ResponseWriter, r *http.Request, purpose string) {
	id, aid := r.URL.Query().Get("i"), r.PathValue("previewId")
	s.mu.Lock()
	p := s.previews[id]
	s.mu.Unlock()
	if p == nil {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	sig := r.URL.Query().Get("t")
	if purpose == "image" {
		sig = r.URL.Query().Get("it")
	}
	if aid != p.assetID || !hmac.Equal([]byte(sig), []byte(s.livePreviewSignature(p, id, purpose))) || (purpose == "preview") != (p.kind == "video") {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	if !time.Now().Before(p.expires) {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	user, e := s.Sessions.User(r.Context(), p.session)
	if e != nil || user != p.user {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	res, e := s.loadPersistentResult(r.Context(), p.user, p.resultID)
	if e != nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if !time.Now().Before(res.ExpiresAt) {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	var version int64
	if e = s.Sessions.DB.QueryRowContext(r.Context(), "SELECT version FROM media_asset WHERE id=? AND user_id=? AND result_id=?", aid, p.user, p.resultID).Scan(&version); e != nil || version != p.version {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	a := sourceAsset(res.result, aid)
	if a == nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if policy, e := s.deliveryPolicy(r.Context(), res.result, asset{ID: aid, Kind: a.Kind, Role: a.Role, Order: a.Order}); e != nil || policy == "copy_only" {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	if len(r.Header.Values("Range")) > 1 || strings.Contains(r.Header.Get("Range"), ",") {
		fail(w, 416, "BAD_REQUEST")
		return
	}
	release, allowed := s.acquireMedia(p.user, id)
	if !allowed {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), s.Settings.Seconds("client_download_timeout_seconds"))
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.Settings.Seconds("client_download_timeout_seconds")))
	counted := &countedMediaResponse{ResponseWriter: w}
	if purpose == "preview" {
		limit := s.Settings.Int("preview_max_bytes")
		stream, err := s.Relay.VideoPreview(ctx, res.Value.Platform, *a, r.Header.Get("Range"), r.Method == "HEAD", limit)
		if err != nil {
			if err.Error() == "BAD_RANGE" {
				fail(w, 416, "BAD_REQUEST")
			} else {
				fail(w, 503, "DIRECT_DOWNLOAD_UNAVAILABLE")
			}
			return
		}
		defer stream.Body.Close()
		if r.Method != "HEAD" {
			settle, err := s.reservePreviewBytes(ctx, p.user, purpose+":"+id, p.expires, stream.Size, limit)
			if err != nil {
				fail(w, 429, "RATE_LIMITED")
				return
			}
			defer func() { settle(counted.sent) }()
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(stream.Size))
		if stream.ContentRange != "" {
			w.Header().Set("Content-Range", stream.ContentRange)
		}
		w.WriteHeader(stream.Status)
		if r.Method != "HEAD" {
			_, _ = io.Copy(counted, stream.Body)
		}
		return
	}
	contentID := ""
	if res.Value.ContentID != nil {
		contentID = *res.Value.ContentID
	}
	var prepared media.Prepared
	var err error
	if purpose == "thumbnail" { // thumbnails have a separate immutable cache version.
		prepared, err = s.Relay.PrepareThumbnail(ctx, res.Value.Platform, contentID, *a)
	} else {
		prepared, err = s.Relay.Prepare(ctx, res.Value.Platform, contentID, s.adapterVersion(), *a)
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	file, done, err := s.Relay.Acquire(prepared.StorageKey)
	if err != nil {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	defer done()
	info, err := file.Stat()
	if err != nil || info.Size() != prepared.Size {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	if r.Method != "HEAD" {
		n, valid := mediaRangeSize(r.Header.Get("Range"), prepared.Size)
		if !valid {
			fail(w, 416, "BAD_REQUEST")
			return
		}
		if r.Header.Get("If-Range") != "" {
			n = prepared.Size
		}
		settle, err := s.reservePreviewBytes(ctx, p.user, purpose+":"+id, p.expires, n, prepared.Size*s.Settings.Int("ticket_byte_budget_multiplier"))
		if err != nil {
			fail(w, 429, "RATE_LIMITED")
			return
		}
		defer func() { settle(counted.sent) }()
	}
	w.Header().Set("Content-Type", prepared.MIME)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", "\""+prepared.Hash+"\"")
	http.ServeContent(counted, r, "image", time.Time{}, file)
}
