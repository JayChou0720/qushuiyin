package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"qushuiyin/service/internal/parse"
	"qushuiyin/service/migrations"
)

func TestDurableTasksMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not configured")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.DBName != "qushuiyin_test" || cfg.Net != "tcp" || !strings.HasPrefix(cfg.Addr, "127.0.0.1:") {
		t.Fatal("requires isolated loopback qushuiyin_test")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	m, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	values, err := config.Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.Store{DB: db, Key: []byte("isolated durable task test HMAC key"), TTL: time.Hour}
	login, err := sessions.DevLogin(ctx, auth.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, q := range cleanupUserQueries {
			if _, e := db.ExecContext(context.Background(), q, login.UserID); e != nil {
				t.Error(e)
			}
		}
		db.Exec("DELETE FROM app_user WHERE id=?", login.UserID)
	}()
	makeServer := func() (*Mock, http.Handler) {
		a, e := parse.Mock(config.Defaults(), parse.ValidationPolicy())
		if e != nil {
			t.Fatal(e)
		}
		s := &Mock{Persistent: true, Settings: values, Sessions: sessions, Adapter: a, Origin: "http://127.0.0.1:18080", TicketKey: []byte("isolated durable ticket key distinct")}
		if e = s.ConfigureTaskKeys("v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); e != nil {
			t.Fatal(e)
		}
		return s, s.Handler(ctx)
	}
	request := func(h http.Handler, method, path, key, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+login.Token)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s got %d: %s", method, path, w.Code, w.Body.String())
		}
		var out struct{ Data map[string]any }
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid JSON")
		}
		return out.Data
	}
	s, h := makeServer()
	key := auth.ID()
	const body = `{"shareText":"https://www.douyin.com/video/10001","source":"manual","flowId":"durable-test"}`
	created := request(h, "POST", "/v1/parse-jobs", key, body, 202)
	id := created["jobId"].(string)
	_, fresh := makeServer()
	if replay := request(fresh, "POST", "/v1/parse-jobs", key, body, 202); replay["jobId"] != id {
		t.Fatal("restart lost idempotency")
	}
	request(fresh, "POST", "/v1/parse-jobs", key, strings.Replace(body, "10001", "10002", 1), 409)
	old, err := s.claimParse(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE parse_job SET lease_expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	current, err := s.claimParse(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	if current.Claim <= old.Claim {
		t.Fatal("claim sequence did not advance")
	}
	parsed, err := s.Adapter.Parse(ctx, current.Source)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.finishParse(ctx, old, parsed, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM parse_result WHERE job_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale worker published a result", err)
	}
	if err = s.finishParse(ctx, current, parsed, nil); err != nil {
		t.Fatal(err)
	}
	restarted, rh := makeServer()
	done := request(rh, "GET", "/v1/parse-jobs/"+id, "", "", 200)
	if done["status"] != "SUCCEEDED" {
		t.Fatalf("result not durable: %v", done)
	}
	rid := done["resultId"].(string)
	res := request(rh, "GET", "/v1/results/"+rid, "", "", 200)
	assets := res["assets"].([]any)
	aid := assets[0].(map[string]any)["assetId"].(string)
	payload, _ := json.Marshal(dto{"resultId": rid, "assetIds": []string{aid}, "clientActionId": auth.ID(), "flowId": "durable-test"})
	bk := auth.ID()
	b := request(rh, "POST", "/v1/save-batches", bk, string(payload), 202)
	bid := b["batchId"].(string)
	next, nh := makeServer()
	if replay := request(nh, "POST", "/v1/save-batches", bk, string(payload), 202); replay["batchId"] != bid {
		t.Fatal("restart lost batch idempotency")
	}
	if worked, e := next.preparePersistentItem(ctx, "media-new"); e != nil || !worked {
		t.Fatal("media item recovery failed", e)
	}
	_, last := makeServer()
	ready := request(last, "GET", "/v1/save-batches/"+bid, "", "", 200)
	if ready["status"] != "READY" {
		t.Fatalf("batch not durable: %v", ready)
	}
	// A cancelled job cannot create a result even if the old resolver succeeds.
	second := request(rh, "POST", "/v1/parse-jobs", auth.ID(), body, 202)["jobId"].(string)
	claim, err := restarted.claimParse(ctx, "cancel-race")
	if err != nil {
		t.Fatal(err)
	}
	request(rh, "DELETE", "/v1/parse-jobs/"+second, "", "", 200)
	if err = restarted.finishParse(ctx, claim, parsed, nil); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM parse_result WHERE job_id=?", second).Scan(&count); err != nil || count != 0 {
		t.Fatal("cancelled worker published a result", err)
	}
}
