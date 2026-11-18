// Package httpapi exposes the local mock API. Its task state is disposable;
// sessions and configuration use the same database boundaries as the service.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/parse"
)

type Platform struct {
	Name   string `json:"platform"`
	Status string `json:"status"`
	Video  bool   `json:"videoEnabled"`
	Image  bool   `json:"imageEnabled"`
	Cover  bool   `json:"coverEnabled"`
	Text   bool   `json:"textEnabled"`
}
type Mock struct {
	Admin          auth.AdminStore
	AdminHost      string
	AdminOrigins   map[string]bool
	adminHashSlots chan struct{}
	Settings       config.Values
	Sessions       auth.Store
	TicketKey      []byte
	Adapter        parse.Adapter
	Platforms      []Platform
	Origin         string
	DevLogin       bool
	lifecycle      sync.RWMutex
	mu             sync.Mutex
	jobs           map[string]*job
	results        map[string]*result
	batches        map[string]*batch
	previews       map[string]*preview
	keys           map[string]idem
	rates          map[string]meter
}
type meter struct {
	Until time.Time
	Count int64
}
type idem struct {
	ID, Digest string
	Expires    time.Time
}
type dto map[string]any
type envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
	Data      any    `json:"data"`
	Error     any    `json:"error,omitempty"`
}
type credential struct{ User, Digest string }
type authed func(http.ResponseWriter, *http.Request, credential)

const privacyVersion = "2026-09-v1"

var messages = map[string]string{"OK": "操作成功", "INPUT_EMPTY": "请粘贴作品分享链接", "INPUT_TOO_LONG": "内容过多，请只粘贴一个作品", "TOO_MANY_LINKS": "链接太多，请只粘贴一个作品", "MULTIPLE_LINKS": "检测到多个链接，请选择一个", "NO_LINK_FOUND": "未找到作品分享链接", "LINK_INVALID": "链接无效，请检查后重试", "LINK_NOT_ALLOWED": "链接无效或暂不支持该地址", "PLATFORM_UNSUPPORTED": "暂不支持这个平台", "PLATFORM_MAINTENANCE": "该平台正在维护，请稍后再试", "CONTENT_TYPE_UNSUPPORTED": "暂不支持该内容类型", "UPSTREAM_UNAVAILABLE": "作品无法访问，可能已删除或限制访问", "UPSTREAM_INVALID_RESPONSE": "没有取得可用的媒体资源", "TOO_MANY_ASSETS": "作品资源过多", "PARSE_TIMEOUT": "解析超时，请重试", "AUTH_REQUIRED": "请先登录", "SESSION_EXPIRED": "使用状态已过期，请重试", "RESOURCE_NOT_FOUND": "资源不存在", "RESULT_EXPIRED": "解析结果已过期，请重新解析", "JOB_EXPIRED": "解析任务已过期", "BATCH_EXPIRED": "保存操作已过期", "BAD_REQUEST": "请求参数有误", "PRIVACY_VERSION_REQUIRED": "请先查看最新隐私说明", "IDEMPOTENCY_CONFLICT": "该操作已使用不同参数提交", "FILE_NOT_READY": "文件尚未准备好", "FALLBACK_NOT_ALLOWED": "当前下载方式不支持回退", "RATE_LIMITED": "操作较频繁，请稍后再试", "SERVICE_BUSY": "服务繁忙，请稍后再试", "INTERNAL_ERROR": "服务暂不可用"}

func issue(code string) dto {
	return dto{"code": code, "message": messageFor(code), "retryable": code == "UPSTREAM_UNAVAILABLE" || code == "PARSE_TIMEOUT" || code == "PREPARE_TIMEOUT" || code == "MEDIA_SOURCE_EXPIRED"}
}
func messageFor(code string) string {
	switch code {
	case "DAILY_LIMIT_REACHED":
		return "今日操作次数已达上限，请稍后再试"
	case "PREPARE_TIMEOUT":
		return "文件准备超时"
	case "MEDIA_SOURCE_EXPIRED":
		return "作品下载地址已失效"
	case "RETRY_NOT_ALLOWED":
		return "此项当前不能重新准备"
	case "RETRY_LIMIT_REACHED":
		return "本次重试次数已用完，请重新解析"
	case "DIRECT_DOWNLOAD_UNAVAILABLE":
		return "暂时无法直接保存，可复制链接到浏览器下载"
	case "ACCESS_DENIED":
		return "当前无法执行此操作"
	case "ADMIN_LOGIN_FAILED":
		return "无法登录，请检查账号和密码或稍后重试"
	}
	if msg := messages[code]; msg != "" {
		return msg
	}
	return messages["INTERNAL_ERROR"]
}
func send(w http.ResponseWriter, status int, code string, data, details any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	rid := auth.ID()
	w.Header().Set("X-Request-Id", rid)
	res := envelope{Code: code, Message: messageFor(code), RequestID: rid, Data: data}
	if code != "OK" {
		res.Error = dto{"retryable": status == 429 || status >= 500, "details": details}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}
func ok(w http.ResponseWriter, status int, data any)      { send(w, status, "OK", data, nil) }
func fail(w http.ResponseWriter, status int, code string) { send(w, status, code, nil, nil) }
func (s *Mock) read(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.Settings.Int("max_request_body_bytes"))
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		fail(w, 400, "BAD_REQUEST")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "BAD_REQUEST")
		return false
	}
	return true
}
func (s *Mock) authorized(next authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" || len(token) > 128 {
			fail(w, 401, "AUTH_REQUIRED")
			return
		}
		c := credential{Digest: s.Sessions.TokenDigest(token)}
		var err error
		c.User, err = s.Sessions.User(r.Context(), c.Digest)
		if errors.Is(err, auth.ErrSession) {
			fail(w, 401, "SESSION_EXPIRED")
			return
		}
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		next(w, r, c)
	}
}
func (s *Mock) minute(key string, cap int64) bool {
	return s.rate(key, cap, time.Minute)
}
func (s *Mock) rate(key string, cap int64, period time.Duration) bool {
	now := time.Now()
	m := s.rates[key]
	if !now.Before(m.Until) {
		if len(s.rates) >= int(s.Settings.Int("memory_rate_limit_max_subjects")) {
			for k, v := range s.rates {
				if !now.Before(v.Until) {
					delete(s.rates, k)
				}
			}
			if len(s.rates) >= int(s.Settings.Int("memory_rate_limit_max_subjects")) {
				return false
			}
		}
		m = meter{Until: now.Add(period)}
	}
	if m.Count >= cap {
		return false
	}
	m.Count++
	s.rates[key] = m
	return true
}
func (s *Mock) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Code, ClientVersion, PrivacyVersion string }
	if !s.read(w, r, &req) {
		return
	}
	name, prefix := strings.CutPrefix(req.Code, "dev:")
	name = strings.TrimSpace(name)
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.DevLogin || !config.LoopbackHost(host) || !prefix || name == "" || utf8.RuneCountInString(name) > 64 {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if req.PrivacyVersion != privacyVersion {
		fail(w, 409, "PRIVACY_VERSION_REQUIRED")
		return
	}
	s.mu.Lock()
	allowed := s.minute("login:"+host, s.Settings.Int("ip_parse_requests_per_minute"))
	s.mu.Unlock()
	if !allowed {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	sess, err := s.Sessions.DevLogin(r.Context(), name)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, sess)
}
func (s *Mock) publicConfig(w http.ResponseWriter, r *http.Request) {
	keys := []string{"max_share_text_chars", "max_candidate_urls", "max_image_batch_count", "client_parse_wait_seconds", "poll_interval_ms", "client_download_timeout_seconds", "max_video_bytes", "max_image_bytes", "feedback_min_chars", "feedback_max_chars", "feedback_contact_max_chars", "pagination_default", "pagination_max", "event_batch_max", "client_queue_max", "client_event_retention_hours"}
	limits := map[string]json.RawMessage{}
	for _, k := range keys {
		limits[k] = s.Settings.Data[k]
	}
	configData := dto{"platforms": s.Platforms, "limits": limits, "features": dto{"mock": true, "devLogin": s.DevLogin, "directPreviewEnabled": false}, "privacyVersion": privacyVersion, "notice": "", "help": dto{}, "serviceStatus": "normal"}
	raw, _ := json.Marshal(configData)
	etag := auth.Digest(s.Sessions.Key, string(raw))
	w.Header().Set("ETag", "\""+etag+"\"")
	if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
		w.WriteHeader(304)
		return
	}
	configData["configVersion"] = etag
	configData["serverTime"] = time.Now().UTC()
	ok(w, 200, configData)
}
func (s *Mock) Handler() http.Handler {
	s.jobs = map[string]*job{}
	s.results = map[string]*result{}
	s.batches = map[string]*batch{}
	s.previews = map[string]*preview{}
	s.keys = map[string]idem{}
	s.rates = map[string]meter{}
	m := http.NewServeMux()
	if s.Admin.DB != nil && s.AdminHost != "" {
		s.adminRoutes(m)
	}
	m.HandleFunc("GET /v1/config", s.publicConfig)
	m.HandleFunc("POST /v1/sessions/wechat", s.login)
	m.HandleFunc("DELETE /v1/sessions/current", s.authorized(func(w http.ResponseWriter, r *http.Request, c credential) {
		if s.Sessions.Revoke(r.Context(), c.Digest) != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		ok(w, 200, dto{})
	}))
	m.HandleFunc("POST /v1/parse-jobs", s.authorized(s.createJob))
	m.HandleFunc("GET /v1/parse-jobs/{jobId}", s.authorized(s.jobStatus))
	m.HandleFunc("DELETE /v1/parse-jobs/{jobId}", s.authorized(s.jobStatus))
	m.HandleFunc("GET /v1/results/{resultId}", s.authorized(s.resultData))
	m.HandleFunc("POST /v1/results/{resultId}/preview-tickets", s.authorized(s.previewTickets))
	m.HandleFunc("POST /v1/save-batches", s.authorized(s.createBatch))
	m.HandleFunc("GET /v1/save-batches/{batchId}", s.authorized(s.batchStatus))
	m.HandleFunc("DELETE /v1/save-batches/{batchId}", s.authorized(s.batchStatus))
	m.HandleFunc("POST /v1/save-batches/{batchId}/tickets", s.authorized(s.tickets))
	m.HandleFunc("POST /v1/save-batches/{batchId}/retry", s.authorized(s.retryBatch))
	m.HandleFunc("POST /v1/save-batches/{batchId}/items/{itemId}/fallback", s.authorized(s.fallbackItem))
	m.HandleFunc("POST /v1/save-batches/{batchId}/items/{itemId}/copy-link", s.authorized(s.copyLink))
	m.HandleFunc("POST /v1/feedbacks", s.authorized(s.submitFeedback))
	m.HandleFunc("GET /v1/feedbacks", s.authorized(s.feedbackList))
	m.HandleFunc("GET /v1/feedbacks/{feedbackId}", s.authorized(s.feedbackDetail))
	m.HandleFunc("POST /v1/events", s.authorized(s.ingestEvents))
	m.HandleFunc("DELETE /v1/me/data", s.authorized(s.deleteData))
	m.HandleFunc("GET /media/download/{mediaObjectId}", s.download("relay"))
	m.HandleFunc("GET /media/mock-source/direct/{mediaObjectId}", s.download("direct"))
	m.HandleFunc("GET /media/mock-source/copy/{mediaObjectId}", s.download("copy"))
	m.HandleFunc("GET /media/preview/{previewId}", s.previewMedia("preview"))
	m.HandleFunc("GET /media/thumbnail/{previewId}", s.previewMedia("thumbnail"))
	m.HandleFunc("GET /media/image/{previewId}", s.previewMedia("thumbnail"))
	m.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fail(w, 404, "RESOURCE_NOT_FOUND") })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Keep a request authenticated just before deletion from repopulating the mock store.
		if r.Method == http.MethodDelete && r.URL.Path == "/v1/me/data" {
			s.lifecycle.Lock()
			defer s.lifecycle.Unlock()
		} else {
			s.lifecycle.RLock()
			defer s.lifecycle.RUnlock()
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Mock) prune(now time.Time) {
	for k, x := range s.previews {
		if !now.Before(x.expires) {
			delete(s.previews, k)
		}
	}
	for k, x := range s.keys {
		if !now.Before(x.Expires) {
			delete(s.keys, k)
		}
	}
	for k, x := range s.jobs {
		if !now.Before(x.Expires) {
			delete(s.jobs, k)
		}
	}
	for k, x := range s.results {
		if !now.Before(x.ExpiresAt) {
			delete(s.results, k)
		}
	}
	for k, x := range s.batches {
		if !now.Before(x.ExpiresAt) {
			delete(s.batches, k)
		}
	}
}
func (s *Mock) replay(w http.ResponseWriter, r *http.Request, c credential, op string, req any, now time.Time) (string, idem, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 100 {
		fail(w, 400, "BAD_REQUEST")
		return "", idem{}, false
	}
	key = c.User + ":" + op + ":" + key
	raw, _ := json.Marshal(req)
	digest := auth.Digest(s.Sessions.Key, string(raw))
	rec := s.keys[key]
	if rec.ID != "" && rec.Digest != digest {
		fail(w, 409, "IDEMPOTENCY_CONFLICT")
		return "", idem{}, false
	}
	if rec.ID == "" {
		if int64(len(s.keys)) >= s.Settings.Int("memory_parse_cache_max_entries") {
			fail(w, 503, "SERVICE_BUSY")
			return "", idem{}, false
		}
		rec.Digest = digest
		rec.Expires = now.Add(s.Settings.Seconds("idempotency_ttl_seconds"))
	}
	return key, rec, true
}
func parseError(err error) string {
	var code parse.Code
	if errors.As(err, &code) && messages[string(code)] != "" {
		return string(code)
	}
	return "UPSTREAM_UNAVAILABLE"
}
func (s *Mock) Clean(ctx context.Context) {
	s.cleanDatabase(ctx, time.Now().UTC())
	ticker := time.NewTicker(s.Settings.Seconds("media_gc_interval_seconds"))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.mu.Lock()
			s.prune(now)
			s.mu.Unlock()
			s.cleanDatabase(ctx, now.UTC())
		}
	}
}
