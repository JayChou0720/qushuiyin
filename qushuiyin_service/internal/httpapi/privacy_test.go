package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
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

func TestMockPrivacyMySQL(t *testing.T) {
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
	defer db.Close()
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
	store := auth.Store{DB: db, Key: []byte("isolated test keys are not credentials"), TTL: time.Minute}
	login := func(name string) auth.Session {
		t.Helper()
		session, err := store.DevLogin(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	handle := (&Mock{Sessions: store, Settings: values}).Handler(t.Context())
	request := func(method, path, token, payload string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(payload))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", auth.ID())
		w := httptest.NewRecorder()
		handle.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d", method, path, w.Code, want)
		}
	}
	name := auth.ID()
	owner, sibling, other := login(name), login(name), login(auth.ID())
	if owner.UserID != sibling.UserID {
		t.Fatal("dev identity did not reuse its user")
	}
	ids := []string{owner.UserID, other.UserID}
	defer func() {
		for _, id := range ids {
			for _, query := range cleanupUserQueries {
				if _, err := db.ExecContext(ctx, query, id); err != nil {
					t.Error(err)
				}
			}
			if _, err := db.ExecContext(ctx, "DELETE FROM cleanup_job WHERE user_id=?", id); err != nil {
				t.Error(err)
			}
			if _, err := db.ExecContext(ctx, "DELETE FROM app_user WHERE id=?", id); err != nil {
				t.Error(err)
			}
		}
	}()
	const source = "https://www.douyin.com/video/10001?sample=private"
	const contact = "mock-contact@example.invalid"
	payload, _ := json.Marshal(feedbackRequest{Category: "suggestion", Content: "Synthetic feedback for a policy check", Contact: contact, IncludeSource: true, SourceURL: source})
	request("POST", "/v1/feedbacks", other.Token, string(payload), 201)
	for n := int64(0); n < values.Int("user_feedback_per_day"); n++ {
		request("POST", "/v1/feedbacks", owner.Token, string(payload), 201)
	}
	request("POST", "/v1/feedbacks", owner.Token, string(payload), 429)
	var storedContact, storedSource string
	if err = db.QueryRowContext(ctx, "SELECT contact_enc,source_url_enc FROM feedback WHERE user_id=? LIMIT 1", owner.UserID).Scan(&storedContact, &storedSource); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(storedContact, "v1:") || !strings.HasPrefix(storedSource, "v1:") || strings.Contains(storedContact, contact) || strings.Contains(storedSource, source) {
		t.Fatal("expected encrypted sensitive fields")
	}
	fixtureID := auth.ID()
	for _, id := range ids {
		_, err = db.ExecContext(ctx, "INSERT INTO media_object(id,user_id,asset_id,file_id,output_version,status,expires_at) VALUES (?,?,?,?,'test','READY',UTC_TIMESTAMP(3))", auth.ID(), id, auth.ID(), fixtureID)
		if err != nil {
			t.Fatal(err)
		}
	}
	eventPayload, _ := json.Marshal(map[string]any{"events": []map[string]any{{"eventId": auth.ID(), "name": "result_viewed", "stage": "result", "outcome": "succeeded", "clientTime": time.Now().UTC(), "clientVersion": "smoke", "osFamily": "devtools"}}})
	request("POST", "/v1/events", owner.Token, string(eventPayload), 200)
	request("POST", "/v1/events", other.Token, string(eventPayload), 200)
	assertCount := func(table, user string, want int) {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE user_id=?", user).Scan(&n); err != nil || n != want {
			t.Fatalf("%s records: got %d, want %d (error: %v)", table, n, want, err)
		}
	}
	assertCount("event_ingest", owner.UserID, 1)
	assertCount("event_ingest", other.UserID, 1)
	request("DELETE", "/v1/me/data", owner.Token, "", 200)
	request("GET", "/v1/feedbacks", sibling.Token, "", 401)
	request("GET", "/v1/feedbacks", other.Token, "", 200)
	assertCount("feedback", owner.UserID, 0)
	assertCount("event_ingest", owner.UserID, 0)
	assertCount("media_object", owner.UserID, 1)
	replacement := login(name)
	ids = append(ids, replacement.UserID)
	if replacement.UserID == owner.UserID {
		t.Fatal("deleted identity was reused")
	}
	var taskID, status string
	if err = db.QueryRowContext(ctx, "SELECT id,status FROM cleanup_job WHERE user_id=?", owner.UserID).Scan(&taskID, &status); err != nil || status != "PENDING" {
		t.Fatalf("expected durable pending deletion: %s %v", status, err)
	}
	restarted := &Mock{Sessions: store, Settings: values}
	restarted.cleanDatabase(ctx, time.Now().UTC())
	if err = db.QueryRowContext(ctx, "SELECT status FROM cleanup_job WHERE id=?", taskID).Scan(&status); err != nil || status != "SUCCEEDED" {
		t.Fatalf("restarted cleanup: %s %v", status, err)
	}
	for _, table := range []string{"app_session", "media_object", "feedback", "event_ingest", "idempotency_record"} {
		assertCount(table, owner.UserID, 0)
	}
	for _, table := range []string{"app_session", "media_object", "feedback", "event_ingest", "idempotency_record"} {
		assertCount(table, other.UserID, 1)
	}
	var erased bool
	if err = db.QueryRowContext(ctx, "SELECT subject_hash IS NULL AND status='deleted' FROM app_user WHERE id=?", owner.UserID).Scan(&erased); err != nil || !erased {
		t.Fatalf("expected anonymized deletion marker: %v", err)
	}
}
