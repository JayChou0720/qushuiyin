package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestAdminMockMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not configured")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil || parsed.DBName != "qushuiyin_test" || parsed.Net != "tcp" || !strings.HasPrefix(parsed.Addr, "127.0.0.1:") {
		t.Fatal("tests require the isolated loopback qushuiyin_test database")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal("cannot connect to test database")
	}
	migration, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Up(ctx); err != nil {
		t.Fatal(err)
	}
	values, err := config.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.AdminStore{DB: db, Key: []byte("only an isolated integration test"), TTL: time.Minute, Lockout: time.Minute, MaxFailures: 2}
	const host = "localhost:18080"
	const origin = "http://localhost:18080"
	handler := (&Mock{Settings: values, Admin: store, AdminHost: host, AdminOrigins: map[string]bool{origin: true}}).Handler(t.Context())
	request := func(method, path, requestHost, site, payload, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://"+requestHost+path, strings.NewReader(payload))
		if site != "" {
			r.Header.Set("Origin", site)
		}
		if token != "" {
			r.AddCookie(&http.Cookie{Name: adminCookie, Value: token})
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got HTTP %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	username := "test-" + auth.ID()
	password := auth.ID()
	credentials := func(user, pw string) string {
		raw, _ := json.Marshal(map[string]string{"username": user, "password": pw})
		return string(raw)
	}
	const login = "/admin/v1/sessions"
	const me = "/admin/v1/me"
	const logout = "/admin/v1/sessions/current"
	payload := credentials(username, password)
	request("POST", login, "other.local", origin, payload, "", 404)
	request("POST", login, host, "https://wrong.invalid", payload, "", 403)
	request("POST", login, host, "", payload, "", 403)
	request("POST", login, host, origin, "{", "", 400)
	request("GET", me, host, "", "", "", 401)
	request("GET", me, host, "", "", "invalid-or-miniapp-token", 401)
	request("POST", login, host, origin, payload, "", 401)
	if err := store.Bootstrap(ctx, username, []byte(password)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, q := range []string{"DELETE s FROM admin_session s JOIN admin_user u ON s.admin_user_id=u.id WHERE u.username=?", "DELETE a FROM admin_audit a JOIN admin_user u ON a.target_id=u.id WHERE u.username=?", "DELETE FROM admin_user WHERE username=?"} {
			if _, err := db.ExecContext(ctx, q, username); err != nil {
				t.Errorf("test cleanup failed: %v", err)
			}
		}
	}()
	if err := store.Bootstrap(ctx, username+"-other", []byte(password)); !errors.Is(err, auth.ErrBootstrapComplete) {
		t.Fatal("bootstrap after first administrator must be refused")
	}
	var stored string
	if err := db.QueryRowContext(ctx, "SELECT password_hash FROM admin_user WHERE username=?", username).Scan(&stored); err != nil || !strings.HasPrefix(stored, "$argon2id$") || strings.Contains(stored, password) {
		t.Fatal("expected encoded Argon2id hash")
	}
	w := request("POST", login, host, origin, payload, "", 200)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Path != "/admin" || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("administrator cookie is missing security attributes")
	}
	tok := cookies[0].Value
	if bytes.Contains(w.Body.Bytes(), []byte(tok)) || bytes.Contains(w.Body.Bytes(), []byte(password)) {
		t.Fatal("credentials must not be in response JSON")
	}
	if err := db.QueryRowContext(ctx, "SELECT token_digest FROM admin_session WHERE token_digest=?", store.TokenDigest(tok)).Scan(&stored); err != nil || stored == tok {
		t.Fatal("administrator session digest missing")
	}
	w = request("GET", me, host, "", "", tok, 200)
	if !bytes.Contains(w.Body.Bytes(), []byte("super_admin")) {
		t.Fatal("administrator role missing")
	}
	request("DELETE", logout, host, "https://wrong.invalid", "", tok, 403)
	request("GET", me, host, "", "", tok, 200)
	w = request("DELETE", logout, host, origin, "", tok, 200)
	if gone := w.Result().Cookies(); len(gone) != 1 || gone[0].MaxAge >= 0 || gone[0].Path != "/admin" {
		t.Fatal("logout did not remove cookie")
	}
	request("GET", me, host, "", "", tok, 401)
	bad := credentials(username, "wrong")
	for range store.MaxFailures {
		request("POST", login, host, origin, bad, "", 401)
	}
	request("POST", login, host, origin, payload, "", 401)
	if _, err := db.ExecContext(ctx, "UPDATE admin_user SET locked_until=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE username=?", username); err != nil {
		t.Fatal(err)
	}
	w = request("POST", login, host, origin, payload, "", 200)
	tok = w.Result().Cookies()[0].Value
	if _, err := db.ExecContext(ctx, "UPDATE admin_session SET expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE token_digest=?", store.TokenDigest(tok)); err != nil {
		t.Fatal(err)
	}
	request("GET", me, host, "", "", tok, 401)
}
