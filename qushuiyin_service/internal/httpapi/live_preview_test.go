package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"qushuiyin/service/internal/auth"
	fixture "qushuiyin/service/testdata/media"
)

func TestPersistentParseFailureMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	x := h.req("POST", "/v1/parse-jobs", auth.ID(), dto{"shareText": "https://www.douyin.com/video/10003", "source": "manual", "flowId": "failed-parse"}, 202)
	p, e := h.s.claimParse(t.Context(), "failed-worker")
	if e != nil || p.Job.ID != x["jobId"] {
		t.Fatal(e)
	}
	parsed, e := h.s.callParser(t.Context(), p.Source)
	if e == nil {
		t.Fatal("expected failure")
	}
	if e = h.s.finishParse(t.Context(), p, parsed, e); e != nil {
		t.Fatal("publish failure", e)
	}
	x = h.req("GET", "/v1/parse-jobs/"+p.Job.ID, "", nil, 200)
	if x["status"] != "FAILED" {
		t.Fatal("failure lost", x)
	}
	x = h.req("POST", "/v1/parse-jobs", auth.ID(), dto{"shareText": "https://www.douyin.com/video/10003", "source": "manual", "flowId": "timeout-parse"}, 202)
	h.db.Exec("UPDATE parse_job SET deadline_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", x["jobId"])
	_, _ = h.s.claimParse(t.Context(), "idle-worker")
	var status string
	h.db.QueryRow("SELECT status FROM parse_job WHERE id=?", x["jobId"]).Scan(&status)
	if status != "FAILED" {
		t.Fatal("idle claim rolled back expiry", status)
	}
}

func TestPersistentParseWorkersPublishFailuresMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.s.Settings.Data["poll_interval_ms"] = json.RawMessage(`10`)
	h.s.Settings.Data["ip_parse_requests_per_minute"] = json.RawMessage(`100`)
	h.s.Settings.Data["user_parse_per_minute"] = json.RawMessage(`100`)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Go(func() { h.s.parseWorker(ctx, auth.ID()) })
	}
	defer func() { cancel(); wg.Wait() }()
	for i := 0; i < 8; i++ {
		x := h.req("POST", "/v1/parse-jobs", auth.ID(), dto{"shareText": "https://www.douyin.com/video/10003", "source": "manual", "flowId": "worker-failure"}, 202)
		deadline := time.Now().Add(3 * time.Second)
		for {
			x = h.req("GET", "/v1/parse-jobs/"+x["jobId"].(string), "", nil, 200)
			if x["status"] == "FAILED" {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatal("failure not published by workers", x["status"])
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func previewURL(h *deliveryHarness, aid string) string {
	h.t.Helper()
	x := h.req("POST", "/v1/results/"+h.rid+"/preview-tickets", "", dto{"assetIds": []string{aid}}, 200)
	return x["previews"].([]any)[0].(map[string]any)["url"].(string)
}
func previewRequest(h *deliveryHarness, method, address, header string, want int) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, address, nil)
	if header != "" {
		r.Header.Set("Range", header)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != want {
		h.t.Fatalf("preview %s: %d %s", method, w.Code, w.Body.String())
	}
	return w
}

func TestLivePreviewVideoMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	if h.s.Settings.Bool("direct_preview_enabled") {
		t.Fatal("direct previews must default off")
	}
	h.s.Settings.Data["preview_max_bytes"] = json.RawMessage(`24`)
	h.register("registered")
	address := previewURL(h, h.aid)
	if !strings.HasPrefix(address, h.s.Origin+"/media/preview/"+h.aid+"?") || h.gets != 0 {
		t.Fatal("ticket fetched source or exposed source")
	}
	w := previewRequest(h, "HEAD", address, "", 206)
	if w.Body.Len() != 0 || h.gets != 0 || w.Header().Get("Content-Length") != "24" {
		t.Fatal("HEAD fetched media")
	}
	w = previewRequest(h, "GET", address, "bytes=0-11", 206)
	if w.Body.Len() != 12 || w.Header().Get("Content-Range") == "" {
		t.Fatal("unbounded preview")
	}
	w = previewRequest(h, "GET", address, "bytes=0-23", 206)
	if w.Body.Len() != 12 || !strings.HasPrefix(w.Header().Get("Content-Range"), "bytes 0-11/") {
		t.Fatal("preview ignored remaining ticket bytes", w.Header(), w.Body.Len())
	}
	previewRequest(h, "GET", address, "bytes=0-11", 429)
	var charged int64
	if e := h.db.QueryRow("SELECT value FROM usage_counter WHERE subject_key=? AND kind='media_egress'", h.user).Scan(&charged); e != nil || charged != 24 {
		t.Fatal("preview accounting", charged, e)
	}
	h.s.Settings.Data["user_media_egress_per_day_bytes"] = json.RawMessage(`30`)
	w = previewRequest(h, "GET", previewURL(h, h.aid), "bytes=0-23", 206)
	if w.Body.Len() != 6 {
		t.Fatal("preview ignored remaining daily bytes", w.Body.Len())
	}
	w = previewRequest(h, "GET", address, "bytes=0-1,5-8", 416)
	if w.Header().Get("Content-Range") != "bytes */"+strconv.FormatInt(h.size, 10) {
		t.Fatal("preview did not report the unsatisfied range size", w.Header())
	}
	u, _ := url.Parse(address)
	q := u.Query()
	q.Set("t", "bad")
	u.RawQuery = q.Encode()
	previewRequest(h, "GET", u.String(), "", 403)
	previewRequest(h, "GET", strings.Replace(address, "/preview/", "/image/", 1), "", 403)
	h.db.Exec("UPDATE media_asset SET version=version+1 WHERE id=?", h.aid)
	previewRequest(h, "GET", address, "", 403)
	result := h.req("GET", "/v1/results/"+h.rid, "", nil, 200)
	if result["assets"].([]any)[0].(map[string]any)["previewUrl"] != nil {
		t.Fatal("result leaked preview")
	}
	h.s.Settings.Data["direct_preview_enabled"] = json.RawMessage(`true`)
	address = previewURL(h, h.aid)
	if !strings.HasPrefix(address, "https://"+h.host+"/") {
		t.Fatal("registered direct preview unavailable")
	}
	h.register("review")
	address = previewURL(h, h.aid)
	if !strings.HasPrefix(address, h.s.Origin+"/media/preview/") {
		t.Fatal("review host direct preview")
	}
	h.req("POST", "/v1/results/"+h.rid+"/preview-tickets", "", dto{"assetIds": []string{h.aid, h.aid}}, 400)
	other, e := h.s.Sessions.DevLogin(t.Context(), "other-preview-"+h.user)
	if e != nil {
		t.Fatal(e)
	}
	h.token = other.Token
	h.req("POST", "/v1/results/"+h.rid+"/preview-tickets", "", dto{"assetIds": []string{h.aid}}, 404)
	h.db.Exec("DELETE FROM app_session WHERE user_id=?", other.UserID)
	h.db.Exec("DELETE FROM app_user WHERE id=?", other.UserID)
}

func TestLivePreviewImagesMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	if _, err := h.db.Exec("UPDATE parse_result SET content_id=NULL WHERE id=? AND user_id=?", h.rid, h.user); err != nil {
		t.Fatal(err)
	}
	result := h.req("GET", "/v1/results/"+h.rid, "", nil, 200)
	aid := result["cover"].(map[string]any)["assetId"].(string)
	mime, data := fixture.File("image")
	h.s.Relay.Client.Transport = durableMediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "HEAD" {
			h.gets++
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	// Preview cache files have no media_object; remove only this harness's files.
	t.Cleanup(func() {
		rows, e := h.db.Query("SELECT id,storage_key FROM media_file")
		if e != nil {
			return
		}
		var ids []string
		for rows.Next() {
			var id, key string
			rows.Scan(&id, &key)
			if f, e := h.s.Relay.Open(key); e == nil {
				f.Close()
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			h.db.Exec("DELETE FROM media_file WHERE id=?", id)
		}
	})
	address := previewURL(h, aid)
	if h.gets != 0 {
		t.Fatal("ticket generated thumbnail early")
	}
	w := previewRequest(h, "GET", address, "", 200)
	cfg, format, e := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
	if e != nil || format != "jpeg" || int64(max(cfg.Width, cfg.Height)) > h.s.Settings.Int("thumbnail_max_edge_px") {
		t.Fatal("bad thumbnail", cfg, format, e)
	}
	previewRequest(h, "GET", address, "", 200)
	previewRequest(h, "GET", address, "", 429)
	if h.gets != 1 {
		t.Fatal("thumbnail cache not shared", h.gets)
	}
	large := strings.Replace(address, "/thumbnail/", "/image/", 1)
	w = previewRequest(h, "GET", large, "", 200)
	if !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatal("large preview changed original")
	}
	previewRequest(h, "GET", large, "", 200)
	if h.gets != 2 {
		t.Fatal("missing content ID should still cache each output for its owner", h.gets)
	}
	u, _ := url.Parse(large)
	q := u.Query()
	q.Set("it", q.Get("t"))
	u.RawQuery = q.Encode()
	previewRequest(h, "GET", u.String(), "", 403)
	var objects int
	h.db.QueryRow("SELECT COUNT(*) FROM media_object WHERE user_id=?", h.user).Scan(&objects)
	if objects != 0 {
		t.Fatal("preview created download authorization")
	}
	h.s.mu.Lock()
	for _, p := range h.s.previews {
		p.expires = time.Now().Add(-time.Second)
	}
	h.s.mu.Unlock()
	// Expiry is part of the signature and is checked against current authorization.
	previewRequest(h, "GET", large, "", 403)
	fresh := previewURL(h, aid)
	h.db.Exec("UPDATE app_session SET revoked_at=UTC_TIMESTAMP(3) WHERE user_id=?", h.user)
	previewRequest(h, "GET", fresh, "", 403)
}
