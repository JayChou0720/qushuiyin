package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
)

type configEntry struct {
	Key             string          `json:"key"`
	Value           json.RawMessage `json:"value"`
	Version         int             `json:"version"`
	PublishedAt     time.Time       `json:"publishedAt"`
	Reason          string          `json:"reason"`
	Default         json.RawMessage `json:"default"`
	Unit            string          `json:"unit"`
	Editable        bool            `json:"editable"`
	RestartRequired bool            `json:"restartRequired"`
	Reserved        bool            `json:"reserved"`
}

func displayKey(key string) bool { return oneOf(key, "public_notice", "public_help") }
func editableConfig(role, key string) bool {
	if reservedConfig(key) {
		return false
	}
	switch role {
	case "super_admin":
		return true
	case "tech_admin":
		return !criticalConfig(key)
	case "operator":
		return displayKey(key)
	}
	return false
}

func criticalConfig(key string) bool {
	return strings.HasSuffix(key, "_egress_per_day_bytes") ||
		strings.HasSuffix(key, "_per_minute") || strings.HasSuffix(key, "_per_day") ||
		strings.HasPrefix(key, "upstream_challenge_") || strings.HasPrefix(key, "admin_login_") ||
		strings.Contains(key, "_retention_") ||
		oneOf(key, "admin_lockout_seconds", "session_ttl_seconds", "admin_session_ttl_seconds",
			"max_video_bytes", "relay_max_video_bytes", "max_image_bytes", "max_image_pixels",
			"media_cache_budget_bytes", "disk_high_watermark_percent", "max_request_body_bytes")
}

func configUnit(key string) string {
	switch {
	case displayKey(key):
		return "文本"
	case key == "max_display_text_chars":
		return "字符"
	case key == "direct_preview_enabled":
		return "开关"
	case strings.Contains(key, "_seconds"):
		return "秒"
	case strings.HasSuffix(key, "_hours"):
		return "小时"
	case strings.HasSuffix(key, "_days"):
		return "天"
	case strings.HasSuffix(key, "_ms"):
		return "毫秒"
	case strings.HasSuffix(key, "_bytes"):
		return "字节"
	case strings.HasSuffix(key, "_pixels"):
		return "像素"
	case strings.HasSuffix(key, "_px"):
		return "px"
	case strings.HasSuffix(key, "_percent") || key == "thumbnail_jpeg_quality":
		return "%"
	case strings.HasSuffix(key, "_per_minute"):
		return "次/分钟"
	case strings.HasSuffix(key, "_per_day"):
		return "次/日"
	default:
		return "数值"
	}
}
func configAtStartup(key string) bool {
	return oneOf(key, "parser_workers", "media_workers", "image_convert_workers", "admin_password_workers", "parse_cache_ttl_seconds", "memory_parse_cache_max_entries",
		"database_max_open_connections", "wechat_login_timeout_seconds", "max_redirects", "media_gc_interval_seconds", "http_read_timeout_seconds", "http_write_timeout_seconds", "http_idle_timeout_seconds",
		"upstream_requests_per_minute", "upstream_challenge_window_seconds", "upstream_challenge_threshold", "upstream_challenge_cooldown_seconds", "parser_call_timeout_seconds")
}

const configCurrent = `SELECT c.config_key,c.value_json,c.version,c.published_at,c.reason,d.value_json
FROM system_config c JOIN system_config d ON d.config_key=c.config_key AND d.version=1
WHERE NOT EXISTS (SELECT 1 FROM system_config n WHERE n.config_key=c.config_key AND n.version>c.version)`

func reservedConfig(key string) bool {
	return oneOf(key, "parser_transient_retries", "usage_flush_seconds", "memory_session_cache_max_entries",
		"save_batch_activity_lease_seconds", "user_parse_per_day", "backup_retention_days",
		"client_album_concurrency")
}

type configQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readConfigEntries(ctx context.Context, db configQuery) ([]configEntry, error) {
	rows, err := db.QueryContext(ctx, configCurrent+" ORDER BY c.config_key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []configEntry{}
	for rows.Next() {
		var x configEntry
		if err := rows.Scan(&x.Key, &x.Value, &x.Version, &x.PublishedAt, &x.Reason, &x.Default); err != nil {
			return nil, err
		}
		x.Unit = configUnit(x.Key)
		x.RestartRequired = configAtStartup(x.Key)
		x.Reserved = reservedConfig(x.Key)
		entries = append(entries, x)
	}
	return entries, rows.Err()
}
func (s *Mock) adminConfig(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	entries, err := readConfigEntries(r.Context(), s.Admin.DB)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	visible := []configEntry{}
	for _, x := range entries {
		if a.Role == "operator" && !displayKey(x.Key) {
			continue
		}
		x.Editable = editableConfig(a.Role, x.Key)
		visible = append(visible, x)
	}
	ok(w, 200, dto{"items": visible})
}

func (s *Mock) adminConfigHistory(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	key := r.PathValue("key")
	if a.Role == "operator" && !displayKey(key) {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	page, valid := s.readAdminPage(w, r)
	if !valid {
		return
	}
	query, args := page.query("SELECT c.id,c.value_json,c.version,c.created_at,c.published_at,c.operator_id,c.reason FROM system_config c WHERE c.config_key=?", "c", []any{key})
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		var id, operator, reason string
		var value json.RawMessage
		var version int
		var created, published time.Time
		if rows.Scan(&id, &value, &version, &created, &published, &operator, &reason) != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, dto{"id": id, "value": value, "version": version, "createdAt": created, "publishedAt": published, "operatorId": operator, "reason": reason})
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, page.response(items))
}

func validateConfigValue(x configEntry, value json.RawMessage, confirm bool) error {
	raw := bytes.TrimSpace(value)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return errors.New("配置值不能为空")
	}
	if displayKey(x.Key) {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return errors.New("请输入文本")
		}
		return nil
	}
	if x.Key == "direct_preview_enabled" {
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return errors.New("请输入 true 或 false")
		}
		return nil
	}
	var n, d int64
	if json.Unmarshal(raw, &n) != nil || json.Unmarshal(x.Default, &d) != nil || n < 0 || n > 1<<53-1 || strings.HasSuffix(x.Key, "_seconds") && n > math.MaxInt64/int64(time.Second) ||
		strings.HasSuffix(x.Key, "_hours") && n > math.MaxInt64/int64(time.Hour) {
		return errors.New("请输入允许范围内的整数")
	}
	if (strings.HasSuffix(x.Key, "_percent") || x.Key == "thumbnail_jpeg_quality") && n > 100 {
		return errors.New("数值不得超过 100")
	}
	zeroAllowed := strings.HasSuffix(x.Key, "_percent") || oneOf(x.Key, "parser_transient_retries", "direct_reparse_attempts")
	if n == 0 && !zeroAllowed {
		return errors.New("该数值必须大于 0")
	}
	if x.Key == "parser_transient_retries" && n != 0 {
		return errors.New("当前解析适配器只支持 0 次内部重试")
	}
	if (n > 10*d && d > 0 || n*10 < d && (n != 0 || x.Key != "direct_reparse_attempts")) && !confirm {
		return errors.New("超出默认值的十分之一至十倍，需要超级管理员确认")
	}
	return nil
}

func validateConfigRelations(v config.Values) error {
	get := v.Int
	download := get("client_download_timeout_seconds") + get("ticket_safety_margin_seconds")
	prepare := download + get("batch_prepare_timeout_seconds")
	ticket := get("media_ticket_ttl_seconds")
	switch {
	case get("result_ttl_seconds") < prepare || get("batch_ttl_seconds") < prepare:
		return errors.New("结果与批次有效期须覆盖准备、下载和安全余量")
	case ticket < download || ticket > min(get("media_cache_ttl_seconds"), get("result_ttl_seconds"), get("batch_ttl_seconds")):
		return errors.New("媒体票据有效期须覆盖下载窗口，并受底层媒体期限约束")
	case get("direct_url_assumed_ttl_seconds") < download:
		return errors.New("直连地址预估有效期须覆盖下载窗口")
	case get("media_cache_ttl_seconds") > get("shared_media_max_age_seconds"):
		return errors.New("媒体授权期限不能超过共享文件最长有效期")
	case get("relay_max_video_bytes") > get("max_video_bytes"):
		return errors.New("视频中转限额不能超过视频下载限额")
	case get("max_image_batch_count") > get("max_result_assets"):
		return errors.New("批次图片数不能超过结果图片数")
	case get("feedback_min_chars") > get("feedback_max_chars") || get("feedback_max_chars") > 500:
		return errors.New("反馈字数须满足最小、最大值及数据库上限")
	case get("max_title_chars") > 200:
		return errors.New("标题字数不能超过数据库上限")
	case get("pagination_default") > get("pagination_max"):
		return errors.New("默认分页大小不能超过最大分页大小")
	case get("client_event_retry_initial_seconds") > get("client_event_retry_max_seconds") || get("client_event_retry_max_seconds") >= get("client_event_retention_hours")*3600:
		return errors.New("事件重试间隔须在保留期内")
	case get("parser_call_timeout_seconds") > get("parse_job_timeout_seconds") || get("media_fetch_timeout_seconds") > get("batch_prepare_timeout_seconds"):
		return errors.New("单次工作时限不能超过任务总预算")
	case get("user_media_connections") > get("global_media_connections") || get("ticket_connections") > get("user_media_connections"):
		return errors.New("票据、用户和全局连接上限须依次递增")
	case get("user_media_egress_per_day_bytes") > get("global_media_egress_per_day_bytes"):
		return errors.New("用户出站额度不能超过全局额度")
	}
	return nil
}

func (s *Mock) adminConfigUpdate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct {
		Reason       string
		ConfirmRange bool
		Changes      []struct {
			Key             string
			ExpectedVersion int
			Value           json.RawMessage
		}
	}
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	if !valid || len(req.Changes) == 0 {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	s.configGate.Lock()
	defer s.configGate.Unlock()
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	entries, err := readConfigEntries(ctx, tx)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	byKey := map[string]configEntry{}
	next := config.Values{Data: map[string]json.RawMessage{}, Versions: map[string]int{}}
	for _, x := range entries {
		byKey[x.Key] = x
		next.Data[x.Key] = x.Value
		next.Versions[x.Key] = x.Version
	}
	changed := map[string]bool{}
	for _, change := range req.Changes {
		x, exists := byKey[change.Key]
		if !exists || changed[change.Key] || change.ExpectedVersion < 1 {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		if !editableConfig(a.Role, change.Key) {
			fail(w, 403, "ACCESS_DENIED")
			return
		}
		if x.Version != change.ExpectedVersion {
			fail(w, 409, "VERSION_CONFLICT")
			return
		}
		if err := validateConfigValue(x, change.Value, a.Role == "super_admin" && req.ConfirmRange); err != nil {
			send(w, 400, "BAD_REQUEST", nil, dto{"key": change.Key, "reason": err.Error()})
			return
		}
		changed[x.Key] = true
		next.Data[x.Key] = change.Value
		next.Versions[x.Key]++
	}
	if err := validateConfigRelations(next); err != nil {
		send(w, 400, "BAD_REQUEST", nil, dto{"reason": err.Error()})
		return
	}
	for _, key := range []string{"public_notice", "public_help"} {
		if int64(utf8.RuneCountInString(next.Text(key))) > next.Int("max_display_text_chars") {
			send(w, 400, "BAD_REQUEST", nil, dto{"key": key, "reason": "展示文案超过 max_display_text_chars 字符上限"})
			return
		}
	}
	for _, change := range req.Changes {
		x := byKey[change.Key]
		_, err = tx.ExecContext(ctx, "INSERT INTO system_config(id,config_key,value_json,version,published_at,operator_id,reason) VALUES (?,?,?,?,UTC_TIMESTAMP(3),?,?)",
			auth.ID(), x.Key, string(change.Value), next.Versions[x.Key], a.ID, reason)
		if err == nil {
			err = auditChange(ctx, tx, a.ID, "config_publish", "system_config", x.Key, reason, requestID(w), dto{"version": x.Version, "value": x.Value}, dto{"version": next.Versions[x.Key], "value": change.Value})
		}
		if err != nil {
			adminAccountError(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	s.settingsSnapshot.Store(&next)
	ok(w, 200, dto{"versions": next.Versions})
}
