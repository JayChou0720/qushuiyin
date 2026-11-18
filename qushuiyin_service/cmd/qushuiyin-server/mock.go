package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/httpapi"
	"qushuiyin/service/internal/parse"
)

func mockCommand(ctx context.Context, command string, getenv func(string) string, out io.Writer) error {
	env := getenv("APP_ENV")
	if (env != "local" && env != "test") || getenv("PARSE_MODE") != "mock" {
		return errors.New("mock command requires APP_ENV=local or test and PARSE_MODE=mock")
	}
	addr := getenv("HTTP_ADDR")
	host, port, e := net.SplitHostPort(addr)
	if e != nil || !config.LoopbackHost(host) || port == "" || port == "0" {
		return errors.New("HTTP_ADDR must be an explicit loopback address and port")
	}
	origin := getenv("MOCK_MEDIA_ORIGIN")
	if origin == "" {
		origin = "http://" + addr
	}
	u, e := config.LoopbackOrigin(origin)
	if e != nil || u.Host != addr {
		return errors.New("M1 mock media origin must match HTTP_ADDR")
	}
	dsn := getenv("MYSQL_DSN")
	if dsn == "" {
		return errors.New("MYSQL_DSN required")
	}
	database, e := mysql.ParseDSN(dsn)
	if e != nil {
		return errors.New("invalid database configuration")
	}
	database.ParseTime = true
	database.Loc = time.UTC
	db, e := sql.Open("mysql", database.FormatDSN())
	if e != nil {
		return errors.New("invalid database configuration")
	}
	defer db.Close()
	var name string
	if e = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); e != nil || (name != "qushuiyin_dev" && name != "qushuiyin_test") {
		return errors.New("mock mode requires a reachable qushuiyin_dev or qushuiyin_test database")
	}
	if command == "seed" {
		if e = seedMock(ctx, db, u.Hostname()); e != nil {
			return errors.New("seed failed; run migrate up first")
		}
		fmt.Fprintln(out, "mock seed ready")
		return nil
	}
	key, e := base64.StdEncoding.DecodeString(getenv("HMAC_KEY"))
	if e != nil || len(key) < 32 {
		return errors.New("HMAC_KEY must be base64 and at least 32 bytes")
	}
	ticketKey, e := base64.StdEncoding.DecodeString(getenv("TICKET_SIGNING_KEY"))
	if e != nil || len(ticketKey) < 32 || string(ticketKey) == string(key) {
		return errors.New("set a separate base64 TICKET_SIGNING_KEY (at least 32 bytes)")
	}
	settings, e := config.Load(ctx, db)
	if e != nil {
		return errors.New("configuration unavailable; run migrate up first")
	}
	if e = settings.Validate("session_ttl_seconds", "max_request_body_bytes", "max_share_text_chars", "max_candidate_urls", "max_image_batch_count", "parse_job_timeout_seconds", "poll_interval_ms", "result_ttl_seconds", "batch_ttl_seconds", "batch_prepare_timeout_seconds", "client_download_timeout_seconds", "ticket_safety_margin_seconds", "media_ticket_ttl_seconds", "idempotency_ttl_seconds", "media_gc_interval_seconds", "user_active_parse_jobs", "parser_workers", "parse_cache_ttl_seconds", "user_active_save_batches", "memory_parse_cache_max_entries", "memory_rate_limit_max_subjects", "ip_parse_requests_per_minute", "user_parse_per_minute", "user_download_url_requests_per_minute"); e != nil {
		return e
	}
	platforms, policy, e := mockPlatforms(ctx, db)
	if e != nil {
		return errors.New("platform configuration unavailable")
	}
	l := config.Defaults()
	l.Assets = int(settings.Int("max_result_assets"))
	l.Title = int(settings.Int("max_title_chars"))
	l.Description = int(settings.Int("max_description_chars"))
	l.Job = settings.Seconds("parse_job_timeout_seconds")
	l.ParserCall = settings.Seconds("parser_call_timeout_seconds")
	parser, e := parse.Mock(l, policy)
	if e != nil {
		return errors.New("invalid parser fixtures")
	}
	mock := httpapi.Mock{Settings: settings, Sessions: auth.Store{DB: db, Key: key, TTL: settings.Seconds("session_ttl_seconds")}, TicketKey: ticketKey, Adapter: parser, Platforms: platforms, Origin: origin, DevLogin: env == "local" && getenv("WECHAT_DEV_LOGIN") == "true"}
	if err := settings.Validate("admin_session_ttl_seconds", "admin_login_max_failures", "admin_lockout_seconds", "preview_max_bytes", "media_cache_ttl_seconds", "direct_url_assumed_ttl_seconds", "media_prepare_max_attempts_per_item", "user_copy_link_per_day"); err != nil {
		return err
	}
	mock.Admin = auth.AdminStore{DB: db, Key: key, TTL: settings.Seconds("admin_session_ttl_seconds"), MaxFailures: settings.Int("admin_login_max_failures"), Lockout: settings.Seconds("admin_lockout_seconds")}
	if err := settings.Validate("feedback_min_chars", "feedback_max_chars", "feedback_contact_max_chars", "feedback_retention_days", "feedback_closed_retention_days", "user_feedback_per_day", "pagination_default", "pagination_max", "client_queue_max", "event_batch_max", "client_event_retention_hours", "client_event_retry_initial_seconds", "client_event_retry_max_seconds", "diagnostic_retention_days", "cleanup_completion_hours", "admin_audit_retention_days"); err != nil {
		return err
	}
	if settings.Int("client_event_retry_max_seconds") < settings.Int("client_event_retry_initial_seconds") || settings.Int("client_event_retry_max_seconds")/3600 >= settings.Int("client_event_retention_hours") {
		return errors.New("invalid client event retry window")
	}
	mock.AdminHost = getenv("ADMIN_PUBLIC_HOST")
	if mock.AdminHost == "" {
		mock.AdminHost = addr
	}
	mock.AdminOrigins = map[string]bool{origin: true}
	for _, v := range strings.Split(getenv("ADMIN_ALLOWED_ORIGINS"), ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, err := config.LoopbackOrigin(v); err != nil {
			return errors.New("mock admin origins must be loopback HTTP origins")
		}
		mock.AdminOrigins[v] = true
	}
	// All requests are bounded by the job budget; only small sample files are served.
	mock.Persistent = true
	if e = mock.ConfigureTaskKeys(getenv("DATA_ENCRYPTION_KEYS")); e != nil {
		return e
	}
	srv := http.Server{Addr: addr, Handler: mock.Handler(ctx), ReadTimeout: l.Job, WriteTimeout: l.Job, IdleTimeout: settings.Seconds("config_client_cache_seconds")}
	mock.RunPersistentParse(ctx)
	mock.RunPersistentMedia(ctx)
	listener, e := net.Listen("tcp", addr)
	if e != nil {
		return errors.New("local HTTP port is unavailable")
	}
	go mock.Clean(ctx)
	go func() { <-ctx.Done(); _ = srv.Close() }()
	fmt.Fprintln(out, "mock API listening at "+origin)
	if e = srv.Serve(listener); errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func seedMock(ctx context.Context, db *sql.DB, host string) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "UPDATE platform_config SET status='enabled',capabilities=JSON_SET(capabilities,'$.videoEnabled',CAST('true' AS JSON),'$.imageEnabled',CAST(IF(platform='bilibili','false','true') AS JSON),'$.coverEnabled',CAST('true' AS JSON),'$.textEnabled',CAST('true' AS JSON)),version=version+1 WHERE platform IN ('douyin','xiaohongshu','kuaishou','bilibili') AND status='disabled'")
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO download_domain(id,platform,host,status,first_seen_at,last_seen_at,created_at,updated_at,seen_count,registered_at) VALUES (?,'mock',?,'registered',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),0,UTC_TIMESTAMP(3)) ON DUPLICATE KEY UPDATE status=status", auth.ID(), host)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func mockPlatforms(ctx context.Context, db *sql.DB) ([]httpapi.Platform, map[string]parse.Capability, error) {
	rows, e := db.QueryContext(ctx, "SELECT platform,status,capabilities FROM platform_config ORDER BY platform")
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	var list []httpapi.Platform
	policy := map[string]parse.Capability{}
	for rows.Next() {
		var row httpapi.Platform
		var caps []byte
		if e = rows.Scan(&row.Name, &row.Status, &caps); e != nil {
			return nil, nil, e
		}
		if e = json.Unmarshal(caps, &row); e != nil {
			return nil, nil, e
		}
		policy[row.Name] = parse.Capability{Status: row.Status, Video: row.Video, Image: row.Image, Cover: row.Cover, Text: row.Text}
		list = append(list, row)
	}
	return list, policy, rows.Err()
}
