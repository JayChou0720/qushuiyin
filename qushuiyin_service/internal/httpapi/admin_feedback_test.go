package httpapi

import (
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
	_, operator := seedAdmin("operator")
	_, auditor := seedAdmin("auditor")
	_, viewer := seedAdmin("viewer")
	_, tech := seedAdmin("tech_admin")
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
	request := func(method, path, cookie, bearer, site, body string, want int, code string) (dto, string) {
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
	request("DELETE", "/v1/me/data", "", owner.Token, "", "", 200, "OK")
	request("GET", path, support, "", "", "", 404, "RESOURCE_NOT_FOUND")
	page, _ = request("GET", audit, auditor, "", "", "", 200, "OK")
	if len(page["items"].([]any)) != 4 {
		t.Fatal("redacted audit should remain after feedback deletion")
	}
}
