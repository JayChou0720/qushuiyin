package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"
	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/migrations"
)

func TestAdminFeedbackMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not configured")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil || parsed.DBName != "qushuiyin_test" || parsed.Net != "tcp" || !strings.HasPrefix(parsed.Addr, "127.0.0.1:") {
		t.Fatal("tests require the isolated loopback qushuiyin_test database")
	}
	parsed.ParseTime = true
	parsed.Loc = time.UTC
	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	migration, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migration.Up(ctx); err != nil {
		t.Fatal(err)
	}
	values, err := config.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("isolated test keys are not credentials")
	admin := auth.AdminStore{DB: db, Key: key}
	store := auth.Store{DB: db, Key: key, TTL: time.Minute}
	const host = "127.0.0.1:18080"
	const origin = "http://" + host
	s := Mock{Admin: admin, AdminHost: host, AdminOrigins: map[string]bool{origin: true}, Sessions: store, Settings: values}
	handler := s.Handler(t.Context())
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	seedAdmin := func(role string) (string, string) {
		t.Helper()
		id, token := auth.ID(), auth.ID()
		exec("INSERT INTO admin_user(id,username,password_hash,role,status) VALUES (?,?, 'unusable',?,'active')", id, id, role)
		t.Cleanup(func() {
			for _, q := range []string{"DELETE FROM admin_audit WHERE operator_id=?", "DELETE FROM admin_session WHERE admin_user_id=?", "DELETE FROM admin_user WHERE id=?"} {
				if _, err := db.ExecContext(ctx, q, id); err != nil {
					t.Error(err)
				}
			}
		})
		exec("INSERT INTO admin_session(id,admin_user_id,token_digest,expires_at,ip_hash) VALUES (?,?,?,UTC_TIMESTAMP(3)+INTERVAL 1 MINUTE,?)", auth.ID(), id, admin.TokenDigest(token), strings.Repeat("0", 64))
		return id, token
	}
	supportID, support := seedAdmin("support")
	operatorID, operator := seedAdmin("operator")
	_, auditor := seedAdmin("auditor")
	_, viewer := seedAdmin("viewer")
	techID, tech := seedAdmin("tech_admin")
	superID, super := seedAdmin("super_admin")
	owner, err := store.DevLogin(ctx, auth.ID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range append(append([]string{}, cleanupUserQueries...), "DELETE FROM cleanup_job WHERE user_id=?", "DELETE FROM app_user WHERE id=?") {
			if _, err := db.ExecContext(ctx, q, owner.UserID); err != nil {
				t.Error(err)
			}
		}
	})
	type requestFunc func(method, path, cookie, bearer, site, body string, want int, code string) (dto, string)
	makeRequest := func(t *testing.T) requestFunc {
		return func(method, path, cookie, bearer, site, body string, want int, code string) (dto, string) {
			t.Helper()
			r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
			if cookie != "" {
				r.Header.Set("Cookie", adminCookie+"="+cookie)
			}
			if bearer != "" {
				r.Header.Set("Authorization", "Bearer "+bearer)
			}
			if site != "" {
				r.Header.Set("Origin", site)
			}
			r.Header.Set("Idempotency-Key", auth.ID())
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var res struct {
				Code, RequestID string
				Data            dto
			}
			err := json.Unmarshal(w.Body.Bytes(), &res)
			if err != nil || w.Code != want || res.Code != code || res.RequestID != w.Header().Get("X-Request-Id") {
				t.Fatalf("%s %s: got %d %s, want %d %s: %v", method, path, w.Code, res.Code, want, code, err)
			}
			return res.Data, res.RequestID
		}
	}
	request := makeRequest(t)
	const raw = `{"category":"download_failed","content":"Synthetic private feedback","contact":"feedback@example.invalid","includeSource":true,"sourceUrl":"https://www.douyin.com/video/10001?signature=local-mock"}`
	posted, _ := request("POST", "/v1/feedbacks", "", owner.Token, "", raw, 201, "OK")
	id := posted["feedbackId"].(string)
	path := "/admin/v1/feedbacks/" + id
	audit := "/admin/v1/audit-logs?targetType=feedback&targetId=" + id
	request("GET", path, "", owner.Token, "", "", 401, "AUTH_REQUIRED")
	for _, token := range []string{auditor, viewer, tech} {
		request("GET", path, token, "", "", "", 403, "ACCESS_DENIED")
	}
	request("GET", audit, support, "", "", "", 403, "ACCESS_DENIED")
	data, _ := request("GET", path, support, "", "", "", 200, "OK")
	if data["version"] != float64(1) || data["status"] != "pending" || data["contact"] != nil || data["sourceUrl"] != nil || data["userId"] != nil {
		t.Fatalf("unexpected administrator feedback projection: %v", data)
	}
	list, _ := request("GET", "/admin/v1/feedbacks?status=pending&category=download_failed", operator, "", "", "", 200, "OK")
	var found bool
	for _, item := range list["items"].([]any) {
		if item.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatal("pending feedback not found in matching filter")
	}
	update := func(version int, status, reply string) string {
		fields := dto{"expectedVersion": version, "reason": "Local review"}
		if status != "" {
			fields["status"] = status
		}
		if reply != "" {
			fields["reply"] = reply
		}
		payload, _ := json.Marshal(fields)
		return string(payload)
	}
	request("PATCH", path, support, "", "https://wrong.invalid", update(1, "processing", ""), 403, "ACCESS_DENIED")
	request("PATCH", path, viewer, "", origin, update(1, "processing", ""), 403, "ACCESS_DENIED")
	request("PATCH", path, support, "", origin, update(1, "replied", ""), 400, "BAD_REQUEST")
	request("PATCH", path, support, "", origin, update(1, "processing", ""), 200, "OK")
	request("PATCH", path, operator, "", origin, update(2, "replied", "First private reply"), 200, "OK")
	request("PATCH", path, support, "", origin, update(2, "replied", "Stale reply"), 409, "VERSION_CONFLICT")
	latest, rid := request("PATCH", path, support, "", origin, update(3, "", "Latest private reply"), 200, "OK")
	request("PATCH", path, support, "", origin, update(4, "", "Latest private reply"), 200, "OK")
	userPath := "/v1/feedbacks/" + id
	current, _ := request("GET", userPath, "", owner.Token, "", "", 200, "OK")
	if latest["version"] != float64(4) || current["reply"] != "Latest private reply" || current["status"] != "replied" || current["repliedAt"] != latest["repliedAt"] {
		t.Fatal("owner did not see the latest reply")
	}
	page, _ := request("GET", audit+"&limit=1", auditor, "", "", "", 200, "OK")
	if page["hasMore"] != true {
		t.Fatal("audit did not paginate")
	}
	tail, _ := request("GET", audit+"&cursor="+url.QueryEscape(page["nextCursor"].(string)), auditor, "", "", "", 200, "OK")
	if len(tail["items"].([]any)) != 2 {
		t.Fatal("failed and no-op writes must not add audit entries")
	}
	seen := map[string]bool{}
	var matchedUpdate bool
	for _, result := range []dto{page, tail} {
		for _, item := range result["items"].([]any) {
			entry := item.(map[string]any)
			id := entry["id"].(string)
			if seen[id] {
				t.Fatal("audit pagination repeated an entry")
			}
			seen[id] = true
			if entry["requestId"] == rid {
				matchedUpdate = entry["operatorId"] == supportID && entry["before"].(map[string]any)["version"] == float64(3) && entry["after"].(map[string]any)["replyChanged"] == true
			}
		}
	}
	if !matchedUpdate {
		t.Fatal("audit lacks actor, request, or change metadata")
	}
	redacted, _ := json.Marshal([]dto{page, tail})
	for _, private := range []string{"private reply", "Synthetic private feedback", "example.invalid", "local-mock", owner.UserID} {
		if strings.Contains(string(redacted), private) {
			t.Fatal("audit exposed private feedback data")
		}
	}
	request("PATCH", path, support, "", origin, update(4, "closed", ""), 200, "OK")
	request("PATCH", path, support, "", origin, update(5, "processing", ""), 409, "FEEDBACK_CLOSED")
	var expires time.Time
	if err = db.QueryRowContext(ctx, "SELECT expires_at FROM feedback WHERE id=?", id).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	limit := time.Now().UTC().AddDate(0, 0, int(values.Int("feedback_closed_retention_days")))
	if expires.After(limit) || expires.Before(limit.Add(-time.Minute)) {
		t.Fatal("close did not shorten feedback retention")
	}
	privatePath := path + "/private"
	request("POST", privatePath, support, "", origin, `{"reason":"Local review"}`, 403, "ACCESS_DENIED")
	request("POST", privatePath, super, "", "https://wrong.invalid", `{"reason":"Local review"}`, 403, "ACCESS_DENIED")
	private, _ := request("POST", privatePath, super, "", origin, `{"reason":"Local review"}`, 200, "OK")
	if private["contact"] != "feedback@example.invalid" || private["sourceUrl"] != "https://www.douyin.com/video/10001?signature=local-mock" {
		t.Fatal("admin private projection omitted consented attachment")
	}
	privateAudit, _ := request("GET", audit+"&action=feedback_private_view", auditor, "", "", "", 200, "OK")
	encoded, _ := json.Marshal(privateAudit)
	if len(privateAudit["items"].([]any)) != 1 || bytes.Contains(encoded, []byte("example.invalid")) || bytes.Contains(encoded, []byte("signature")) {
		t.Fatal("private access was not audited safely")
	}

	backup, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	backupExec := func(query string, args ...any) {
		t.Helper()
		if _, err := backup.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"app_user", "app_session", "feedback"} {
		ownerColumn := "user_id"
		if table == "app_user" {
			ownerColumn = "id"
		}
		backupExec("CREATE TEMPORARY TABLE restore_"+table+" AS SELECT * FROM "+table+" WHERE "+ownerColumn+"=?", owner.UserID)
		defer backup.ExecContext(ctx, "DROP TEMPORARY TABLE restore_"+table)
	}
	request("DELETE", "/v1/me/data", "", owner.Token, "", "", 200, "OK")
	request("GET", path, support, "", "", "", 404, "RESOURCE_NOT_FOUND")
	page, _ = request("GET", audit, auditor, "", "", "", 200, "OK")
	if len(page["items"].([]any)) != 5 {
		t.Fatal("redacted audit should remain after feedback deletion")
	}
	var manifest bytes.Buffer
	if _, err = ExportDeletions(ctx, db, key, &manifest); err != nil {
		t.Fatal(err)
	}
	// Restore the pre-deletion backup while the restored service is offline.
	backupExec("UPDATE app_user u JOIN restore_app_user b ON u.id=b.id SET u.status=b.status,u.subject_hash=b.subject_hash,u.deleted_at=b.deleted_at")
	backupExec("DELETE s FROM app_session s JOIN restore_app_session b ON s.id=b.id")
	backupExec("INSERT INTO app_session SELECT * FROM restore_app_session")
	backupExec("INSERT INTO feedback SELECT * FROM restore_feedback")
	if _, err = ReplayDeletions(ctx, db, []byte("incorrect key"), bytes.NewReader(manifest.Bytes())); err == nil {
		t.Fatal("an unauthenticated deletion manifest was accepted")
	}
	var restoredStatus string
	if err = db.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=?", owner.UserID).Scan(&restoredStatus); err != nil || restoredStatus != "active" {
		t.Fatal("invalid manifest changed the restore")
	}
	for range 2 {
		if _, err = ReplayDeletions(ctx, db, key, bytes.NewReader(manifest.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	request("GET", userPath, "", owner.Token, "", "", 401, "SESSION_EXPIRED")
	request("GET", path, support, "", "", "", 404, "RESOURCE_NOT_FOUND")
	request("POST", privatePath, super, "", origin, `{"reason":"Local review"}`, 404, "RESOURCE_NOT_FOUND")
	for _, table := range []string{"app_session", "feedback"} {
		var rows int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE user_id=?", owner.UserID).Scan(&rows)
		if err != nil || rows != 0 {
			t.Fatalf("replay left %s rows: %d, %v", table, rows, err)
		}
	}

	t.Run("administration", func(t *testing.T) {
		request := makeRequest(t)
		for _, p := range []struct{ path, role, query string }{
			{"download-domains", tech, "?status=invalid"}, {"platforms", tech, ""},
			{"failures", tech, "?stage=invalid"}, {"config", super, ""},
			{"admin-users", super, "?limit=0"}, {"config/public_notice/versions", operator, "?cursor=invalid"},
		} {
			base := "/admin/v1/" + p.path
			request("GET", base, "", "", "", "", 401, "AUTH_REQUIRED")
			request("GET", base, viewer, "", "", "", 403, "ACCESS_DENIED")
			request("GET", base, p.role, "", "", "", 200, "OK")
			if p.query != "" {
				request("GET", base+p.query, p.role, "", "", "", 400, "BAD_REQUEST")
			}
		}
		request("GET", "/admin/v1/overview", viewer, "", "", "", 200, "OK")
		request("GET", "/admin/v1/overview?from=not-a-date", viewer, "", "", "", 400, "BAD_REQUEST")

		domainID := auth.ID()
		exec("INSERT INTO download_domain(id,platform,host,status,first_seen_at,last_seen_at,created_at,updated_at,seen_count) VALUES (?,'douyin',?,'observed',UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),3)", domainID, strings.ToLower(domainID)+".test")
		t.Cleanup(func() { exec("DELETE FROM download_domain WHERE id=?", domainID) })
		domainPath := "/admin/v1/download-domains/" + domainID
		const registered = `{"expectedVersion":1,"status":"registered","reason":"Test WeChat confirmation"}`
		request("PATCH", domainPath, "", "", origin, registered, 401, "AUTH_REQUIRED")
		request("PATCH", domainPath, operator, "", origin, registered, 403, "ACCESS_DENIED")
		request("PATCH", domainPath, tech, "", origin, `{"status":"unknown"}`, 400, "BAD_REQUEST")
		result, _ := request("PATCH", domainPath, tech, "", origin, registered, 200, "OK")
		if result["status"] != "registered" || result["registeredAt"] == nil {
			t.Fatal("registration was not persisted")
		}
		request("PATCH", domainPath, tech, "", origin, registered, 409, "VERSION_CONFLICT")

		var platformVersion int64
		if err := db.QueryRowContext(ctx, "SELECT version FROM platform_config WHERE platform='douyin'").Scan(&platformVersion); err != nil {
			t.Fatal(err)
		}
		backupExec("CREATE TEMPORARY TABLE restore_platform AS SELECT * FROM platform_config WHERE platform='douyin'")
		defer backupExec("DROP TEMPORARY TABLE restore_platform")
		defer backupExec("UPDATE platform_config p JOIN restore_platform b ON p.id=b.id SET p.status=b.status,p.message=b.message,p.share_domains=b.share_domains,p.version=b.version,p.updated_at=b.updated_at")
		platformPath := "/admin/v1/platforms/douyin"
		platformPayload, _ := json.Marshal(dto{"expectedVersion": platformVersion, "status": "maintenance", "maintenanceMessage": "Test maintenance", "shareDomains": []string{"www.douyin.com"}, "reason": "Exercise settings"})
		request("PATCH", platformPath, "", "", origin, string(platformPayload), 401, "AUTH_REQUIRED")
		request("PATCH", platformPath, operator, "", origin, string(platformPayload), 403, "ACCESS_DENIED")
		request("PATCH", platformPath, tech, "", origin, `{"expectedVersion":1,"shareDomains":["unrelated.invalid"],"reason":"invalid"}`, 400, "BAD_REQUEST")
		platform, _ := request("PATCH", platformPath, tech, "", origin, string(platformPayload), 200, "OK")
		if platform["status"] != "maintenance" || platform["maintenanceMessage"] != "Test maintenance" {
			t.Fatal("platform changes were not returned")
		}

		t.Cleanup(func() { exec("DELETE FROM system_config WHERE operator_id IN (?,?,?)", superID, techID, operatorID) })
		configPath := "/admin/v1/config"
		currentConfig, _ := request("GET", configPath, super, "", "", "", 200, "OK")
		entries := map[string]map[string]any{}
		for _, item := range currentConfig["items"].([]any) {
			row := item.(map[string]any)
			entries[row["key"].(string)] = row
		}
		change := func(key string, value any) string {
			body, _ := json.Marshal(dto{"reason": "Test configuration permissions", "changes": []dto{{"key": key, "expectedVersion": entries[key]["version"], "value": value}}})
			return string(body)
		}
		reserved := entries["parser_transient_retries"]
		if reserved["reserved"] != true || reserved["editable"] != false {
			t.Fatal("reserved key should be read-only even for super_admin")
		}
		request("PATCH", configPath, super, "", origin, change("parser_transient_retries", 0), 403, "ACCESS_DENIED")
		workers := change("parser_workers", entries["parser_workers"]["value"])
		request("PATCH", configPath, operator, "", origin, workers, 403, "ACCESS_DENIED")
		request("PATCH", configPath, tech, "", origin, workers, 200, "OK")
		limitChars := int(values.Int("max_display_text_chars"))
		request("PATCH", configPath, operator, "", origin, change("public_help", strings.Repeat("😀", limitChars+1)), 400, "BAD_REQUEST")
		request("PATCH", configPath, operator, "", origin, change("public_help", strings.Repeat("😀", limitChars)), 200, "OK")
		request("PATCH", configPath, tech, "", origin, change("max_display_text_chars", limitChars-1), 400, "BAD_REQUEST")
		const invalidConfig = `{"reason":"invalid","changes":[{"key":"public_notice","expectedVersion":1,"value":12}]}`
		request("PATCH", configPath, super, "", origin, invalidConfig, 400, "BAD_REQUEST")
		request("PATCH", configPath, tech, "", origin, `{"reason":"no permission","changes":[{"key":"max_video_bytes","expectedVersion":1,"value":104857600}]}`, 403, "ACCESS_DENIED")
		versions, _ := request("GET", configPath+"/public_notice/versions", operator, "", "", "", 200, "OK")
		prior := versions["items"].([]any)[0].(map[string]any)
		publish := func(version float64, value any) string {
			body, _ := json.Marshal(dto{"reason": "M4 local publish", "changes": []dto{{"key": "public_notice", "expectedVersion": version, "value": value}}})
			return string(body)
		}
		version := prior["version"].(float64)
		request("PATCH", configPath, "", "", origin, publish(version, "announcement"), 401, "AUTH_REQUIRED")
		request("PATCH", configPath, super, "", origin, publish(version, "announcement"), 200, "OK")
		request("PATCH", configPath, super, "", origin, publish(version, "stale"), 409, "VERSION_CONFLICT")
		public, _ := request("GET", "/v1/config", "", "", "", "", 200, "OK")
		if public["notice"] != "announcement" {
			t.Fatal("published notice did not reach public config")
		}
		request("PATCH", configPath, super, "", origin, publish(version+1, prior["value"]), 200, "OK")

		const users = "/admin/v1/admin-users"
		username := auth.ID()
		body, _ := json.Marshal(dto{"username": username, "role": "support", "password": auth.ID(), "reason": "Test account"})
		request("POST", users, "", "", origin, string(body), 401, "AUTH_REQUIRED")
		request("POST", users, tech, "", origin, string(body), 403, "ACCESS_DENIED")
		request("POST", users, super, "", origin, `{}`, 400, "BAD_REQUEST")
		account, _ := request("POST", users, super, "", origin, string(body), 201, "OK")
		createdID := account["id"].(string)
		t.Cleanup(func() { exec("DELETE FROM admin_user WHERE id=?", createdID) })
		itemPath := users + "/" + createdID
		const suspend = `{"status":"disabled","reason":"Suspend test account","expectedVersion":1}`
		request("PATCH", itemPath, "", "", origin, suspend, 401, "AUTH_REQUIRED")
		request("PATCH", itemPath, tech, "", origin, suspend, 403, "ACCESS_DENIED")
		request("PATCH", itemPath, super, "", origin, `{}`, 400, "BAD_REQUEST")
		request("PATCH", itemPath, super, "", origin, suspend, 200, "OK")
		request("PATCH", itemPath, super, "", origin, suspend, 409, "VERSION_CONFLICT")
	})
}
