// Package httpapi exposes the local development API with either fixture or live
// parsing. Persistent mode stores tasks, results and save batches in MySQL.
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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/media"
	"qushuiyin/service/internal/parse"
)

type Platform struct {
	Name               string `json:"platform"`
	Status             string `json:"status"`
	Video              bool   `json:"videoEnabled"`
	Image              bool   `json:"imageEnabled"`
	Cover              bool   `json:"coverEnabled"`
	Text               bool   `json:"textEnabled"`
	MaintenanceMessage string `json:"maintenanceMessage,omitempty"`
}
type Mock struct {
	refreshGates     keyedMutex
	hostWindows      map[string]hostWindow
	Persistent       bool
	AdapterTag       string
	batchGates       keyedMutex
	hostGates        keyedMutex
	TaskKeys         map[string][]byte
	TaskKeyVersion   string
	Relay            *media.Relay
	mediaConnections map[string]int64
	mediaWork        chan struct{}
	byteBudgets      map[string]*ticketBudget
	Admin            auth.AdminStore
	AdminHost        string
	AdminOrigins     map[string]bool
	adminHashSlots   chan struct{}
	Settings         config.Values
	ManagedConfig    bool
	settingsSnapshot atomic.Pointer[config.Values]
	configGate       sync.Mutex
	Sessions         auth.Store
	TicketKey        []byte
	Adapter          parse.Adapter
	Platforms        []Platform
	Origin           string
	DevLogin         bool
	lifecycle        sync.RWMutex
	mu               sync.Mutex
	parseContext     context.Context
	parsePool        chan struct{}
	parseCache       *parseCache
	jobs             map[string]*job
	results          map[string]*result
	batches          map[string]*batch
	previews         map[string]*preview
	keys             map[string]idem
	rates            map[string]meter
}
type meter struct {
	Until time.Time
	Count int64
}
type idem struct {
	ID, Digest string
	Expires    time.Time
	Response   *batchAcceptance
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

var messages = map[string]string{"OK": "操作成功", "INPUT_EMPTY": "请粘贴作品分享链接", "INPUT_TOO_LONG": "内容过多，请只粘贴一个作品", "TOO_MANY_LINKS": "链接太多，请只粘贴一个作品", "MULTIPLE_LINKS": "检测到多个链接，请选择一个", "NO_LINK_FOUND": "未找到作品分享链接", "LINK_INVALID": "链接无效，请检查后重试", "LINK_NOT_ALLOWED": "链接无效或暂不支持该地址", "PLATFORM_UNSUPPORTED": "暂不支持这个平台", "PLATFORM_MAINTENANCE": "该平台暂时繁忙，请稍后再试", "CONTENT_TYPE_UNSUPPORTED": "暂不支持该内容类型", "UPSTREAM_UNAVAILABLE": "作品无法访问，可能已删除或限制访问", "UPSTREAM_INVALID_RESPONSE": "没有取得可用的媒体资源", "TOO_MANY_ASSETS": "作品资源过多", "PARSE_TIMEOUT": "解析超时，请重试", "AUTH_REQUIRED": "请先登录", "SESSION_EXPIRED": "使用状态已过期，请重试", "RESOURCE_NOT_FOUND": "资源不存在", "RESULT_EXPIRED": "解析结果已过期，请重新解析", "JOB_EXPIRED": "解析任务已过期", "BATCH_EXPIRED": "保存操作已过期", "BAD_REQUEST": "请求参数有误", "PRIVACY_VERSION_REQUIRED": "请先查看最新隐私说明", "IDEMPOTENCY_CONFLICT": "该操作已使用不同参数提交", "FILE_NOT_READY": "文件尚未准备好", "FALLBACK_NOT_ALLOWED": "当前下载方式不支持回退", "RATE_LIMITED": "操作较频繁，请稍后再试", "SERVICE_BUSY": "服务繁忙，请稍后再试", "INTERNAL_ERROR": "服务暂不可用"}

func issue(code string) dto {
	return dto{"code": code, "message": messageFor(code), "retryable": code == "UPSTREAM_UNAVAILABLE" || code == "PARSE_TIMEOUT" || code == "PREPARE_TIMEOUT" || code == "MEDIA_SOURCE_EXPIRED" || code == "SERVICE_BUSY"}
}
func messageFor(code string) string {
	switch code {
	case "CONTENT_CHANGED":
		return "作品内容已变化，请重新解析"
	case "FILE_TOO_LARGE":
		return "文件超过当前可保存大小"
	case "FORMAT_UNSUPPORTED":
		return "当前格式暂不支持保存"
	case "DAILY_LIMIT_REACHED":
		return "今日操作次数已达上限，请稍后再试"
	case "VERSION_CONFLICT":
		return "记录已更新，请刷新后重试"
	case "LAST_SUPER_ADMIN":
		return "请至少保留一个启用的超级管理员"
	case "FEEDBACK_CLOSED":
		return "该工单已关闭"
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
	rid := requestID(w)
	res := envelope{Code: code, Message: messageFor(code), RequestID: rid, Data: data}
	if code != "OK" {
		res.Error = dto{"retryable": status == 429 || status >= 500, "details": details}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}
func ok(w http.ResponseWriter, status int, data any)      { send(w, status, "OK", data, nil) }
func fail(w http.ResponseWriter, status int, code string) { send(w, status, code, nil, nil) }
func requestID(w http.ResponseWriter) string {
	id := w.Header().Get("X-Request-Id")
	if id == "" {
		id = auth.ID()
		w.Header().Set("X-Request-Id", id)
	}
	return id
}
func (s *Mock) read(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.settings().Int("max_request_body_bytes"))
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
	return s.rateUntil(key, cap, time.Now().Add(time.Minute))
}
func (s *Mock) rateUntil(key string, cap int64, expires time.Time) bool {
	now := time.Now()
	m := s.rates[key]
	if !now.Before(m.Until) {
		if len(s.rates) >= int(s.settings().Int("memory_rate_limit_max_subjects")) {
			for k, v := range s.rates {
				if !now.Before(v.Until) {
					delete(s.rates, k)
				}
			}
			if len(s.rates) >= int(s.settings().Int("memory_rate_limit_max_subjects")) {
				return false
			}
		}
		m = meter{Until: expires}
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
	allowed := s.minute("login:"+host, s.settings().Int("ip_login_requests_per_minute"))
	s.mu.Unlock()
	if !allowed {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	store := s.Sessions
	if s.ManagedConfig {
		store.TTL = s.settings().Seconds("session_ttl_seconds")
	}
	sess, err := store.DevLogin(r.Context(), name)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, sess)
}
func (s *Mock) publicConfig(w http.ResponseWriter, r *http.Request) {
	settings := s.settings()
	keys := []string{"max_share_text_chars", "max_candidate_urls", "max_image_batch_count", "max_title_chars", "max_description_chars", "config_client_cache_seconds", "client_album_concurrency", "client_parse_wait_seconds", "poll_interval_ms", "client_download_timeout_seconds", "max_video_bytes", "max_image_bytes", "feedback_min_chars", "feedback_max_chars", "feedback_contact_max_chars", "pagination_default", "pagination_max", "event_batch_max", "client_queue_max", "client_event_retention_hours", "client_event_retry_initial_seconds", "client_event_retry_max_seconds", "history_max_count", "history_ttl_days", "thumbnail_local_budget_bytes"}
	limits := map[string]json.RawMessage{}
	for _, k := range keys {
		limits[k] = settings.Data[k]
	}
	platforms := s.Platforms
	if s.ManagedConfig {
		entries, err := loadManagedPlatforms(r.Context(), s.Sessions.DB)
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		platforms = make([]Platform, 0, len(entries))
		for _, p := range entries {
			c := p.Capabilities
			platforms = append(platforms, Platform{Name: p.Platform, Status: p.Status, Video: c.VideoEnabled, Image: c.ImageEnabled, Cover: c.CoverEnabled, Text: c.TextEnabled, MaintenanceMessage: p.MaintenanceMessage})
		}
	}
	configData := dto{"platforms": platforms, "limits": limits, "features": dto{"mock": s.Relay == nil, "devLogin": s.DevLogin, "directPreviewEnabled": settings.Bool("direct_preview_enabled")}, "privacyVersion": privacyVersion, "notice": settings.Text("public_notice"), "help": dto{"message": settings.Text("public_help")}, "serviceStatus": "normal"}
	raw, _ := json.Marshal(configData)
	versions, _ := json.Marshal(settings.Versions)
	etag := auth.Digest(s.Sessions.Key, string(raw)+string(versions))
	w.Header().Set("ETag", "\""+etag+"\"")
	if r.Header.Get("If-None-Match") == w.Header().Get("ETag") {
		w.WriteHeader(304)
		return
	}
	configData["configVersion"] = etag
	configData["serverTime"] = time.Now().UTC()
	ok(w, 200, configData)
}

func (s *Mock) settings() config.Values {
	if current := s.settingsSnapshot.Load(); current != nil {
		return *current
	}
	return s.Settings
}
func (s *Mock) Handler(ctx context.Context) http.Handler {
	if s.Relay != nil && s.ManagedConfig {
		s.Relay.CurrentSettings = s.settings
	}
	s.hostWindows = map[string]hostWindow{}
	s.parseContext = ctx
	if workers := s.settings().Int("parser_workers"); workers > 0 {
		s.parsePool = make(chan struct{}, workers)
	}
	s.parseCache = newParseCache(s.settings().Seconds("parse_cache_ttl_seconds"), int(s.settings().Int("memory_parse_cache_max_entries")))
	s.jobs = map[string]*job{}
	s.results = map[string]*result{}
	s.batches = map[string]*batch{}
	s.previews = map[string]*preview{}
	s.keys = map[string]idem{}
	s.rates = map[string]meter{}
	s.mediaConnections = map[string]int64{}
	s.mediaWork = make(chan struct{}, s.settings().Int("media_workers"))
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
	m.HandleFunc("POST /v1/save-batches", s.authorizedBatch(s.createBatch))
	m.HandleFunc("GET /v1/save-batches/{batchId}", s.authorizedBatch(s.batchStatus))
	m.HandleFunc("DELETE /v1/save-batches/{batchId}", s.authorizedBatch(s.batchStatus))
	m.HandleFunc("POST /v1/save-batches/{batchId}/tickets", s.authorizedBatch(s.tickets))
	m.HandleFunc("POST /v1/save-batches/{batchId}/retry", s.authorizedBatch(s.retryBatch))
	m.HandleFunc("POST /v1/save-batches/{batchId}/items/{itemId}/fallback", s.authorizedBatch(s.fallbackItem))
	m.HandleFunc("POST /v1/save-batches/{batchId}/items/{itemId}/copy-link", s.authorizedBatch(s.copyLink))
	m.HandleFunc("POST /v1/feedbacks", s.authorized(s.submitFeedback))
	m.HandleFunc("GET /v1/feedbacks", s.authorized(s.feedbackList))
	m.HandleFunc("GET /v1/feedbacks/{feedbackId}", s.authorized(s.feedbackDetail))
	m.HandleFunc("POST /v1/events", s.authorized(s.ingestEvents))
	m.HandleFunc("GET /v1/me", s.authorized(func(w http.ResponseWriter, _ *http.Request, c credential) {
		ok(w, 200, dto{"userId": c.User})
	}))
	m.HandleFunc("DELETE /v1/me/data", s.authorized(s.deleteData))
	m.HandleFunc("GET /media/download/{mediaObjectId}", s.download("relay"))
	m.HandleFunc("GET /media/mock-source/direct/{mediaObjectId}", s.download("direct"))
	m.HandleFunc("GET /media/mock-source/copy/{mediaObjectId}", s.download("copy"))
	m.HandleFunc("GET /media/preview/{previewId}", s.previewMedia("preview"))
	m.HandleFunc("GET /media/thumbnail/{previewId}", s.previewMedia("thumbnail"))
	m.HandleFunc("GET /media/image/{previewId}", s.previewMedia("image"))
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
			x.cancel()
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
	return s.replayWithKeys(w, r, c, op, req, now, s.keys)
}
func (s *Mock) replayWithKeys(w http.ResponseWriter, r *http.Request, c credential, op string, req any, now time.Time, keys map[string]idem) (string, idem, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 100 {
		fail(w, 400, "BAD_REQUEST")
		return "", idem{}, false
	}
	key = c.User + ":" + op + ":" + key
	raw, _ := json.Marshal(req)
	digest := auth.Digest(s.Sessions.Key, string(raw))
	rec := keys[key]
	if rec.ID != "" && rec.Digest != digest {
		fail(w, 409, "IDEMPOTENCY_CONFLICT")
		return "", idem{}, false
	}
	if rec.ID == "" {
		if !s.Persistent && int64(len(keys)) >= s.settings().Int("memory_mock_max_entries") {
			fail(w, 503, "SERVICE_BUSY")
			return "", idem{}, false
		}
		rec.Digest = digest
		rec.Expires = now.Add(s.settings().Seconds("idempotency_ttl_seconds"))
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
	ticker := time.NewTicker(s.settings().Seconds("media_gc_interval_seconds"))
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
