package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
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
	"qushuiyin/service/internal/media"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"qushuiyin/service/migrations"
	fixture "qushuiyin/service/testdata/media"
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
	cleanupUser := func(user string) {
		for _, q := range cleanupUserQueries {
			if _, e := db.ExecContext(context.Background(), q, user); e != nil {
				t.Error(e)
			}
		}
		db.Exec("DELETE FROM cleanup_job WHERE user_id=?", user)
		db.Exec("DELETE FROM app_user WHERE id=?", user)
	}
	defer cleanupUser(login.UserID)
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
	requestAs := func(h http.Handler, token, method, path, key, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
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
	request := func(h http.Handler, method, path, key, body string, want int) map[string]any {
		t.Helper()
		return requestAs(h, login.Token, method, path, key, body, want)
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
	t.Run("expired sources are erased without losing idempotency", func(t *testing.T) {
		key := auth.ID()
		expiredJob := request(rh, "POST", "/v1/parse-jobs", key, body, 202)["jobId"].(string)
		p, e := restarted.claimParse(ctx, "retention-test")
		if e != nil {
			t.Fatal(e)
		}
		if e = restarted.finishParse(ctx, p, parsed, nil); e != nil {
			t.Fatal(e)
		}
		var expiredResult string
		if e = db.QueryRow("SELECT result_id FROM parse_job WHERE id=?", expiredJob).Scan(&expiredResult); e != nil {
			t.Fatal(e)
		}
		for _, q := range []struct{ query, id string }{
			{"UPDATE parse_job SET expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND,source_expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", expiredJob},
			{"UPDATE parse_result SET expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", expiredResult},
			{"UPDATE media_asset SET source_expiry=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE result_id=?", expiredResult},
		} {
			if _, e = db.Exec(q.query, q.id); e != nil {
				t.Fatal(e)
			}
		}
		restarted.cleanDatabase(ctx, time.Now().UTC())
		var retained int
		if e = db.QueryRow(`SELECT (SELECT COUNT(*) FROM parse_job WHERE id=? AND source_url_enc IS NOT NULL)+(SELECT COUNT(*) FROM parse_result WHERE id=? AND history_url_enc IS NOT NULL)+(SELECT COUNT(*) FROM media_asset WHERE result_id=? AND source_candidates_enc<>'')`, expiredJob, expiredResult, expiredResult).Scan(&retained); e != nil || retained != 0 {
			t.Fatal("expired source signatures retained", retained, e)
		}
		_, fresh := makeServer()
		request(fresh, "GET", "/v1/parse-jobs/"+expiredJob, "", "", 410)
		request(fresh, "GET", "/v1/results/"+expiredResult, "", "", 410)
		request(fresh, "POST", "/v1/parse-jobs", key, body, 410)
	})
	t.Run("relay bytes sharing tickets and cleanup", func(t *testing.T) {
		probe := os.Getenv("FFPROBE_TEST_PATH")
		if probe == "" {
			t.Skip("FFPROBE_TEST_PATH not configured")
		}
		downloads := 0
		started, resume := make(chan struct{}), make(chan struct{})
		relay := &media.Relay{DB: db, Directory: t.TempDir(), FFprobe: probe, Settings: values, Key: []byte(auth.ID()), Client: outbound.Client{Redirects: 5, Transport: durableMediaTransport(func(r *http.Request) (*http.Response, error) {
			downloads++
			if downloads == 1 {
				close(started)
				<-resume
			}
			mime, body := fixture.File("video")
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{mime}}, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body))}, nil
		})}}
		live, lh := makeServer()
		live.Relay = relay
		makeBatch := func() string {
			p, _ := json.Marshal(dto{"resultId": rid, "assetIds": []string{aid}, "clientActionId": auth.ID(), "flowId": "durable-test"})
			return request(lh, "POST", "/v1/save-batches", auth.ID(), string(p), 202)["batchId"].(string)
		}
		firstBatch := makeBatch()
		finished := make(chan error, 1)
		go func() { _, e := live.preparePersistentItem(ctx, "relay-old"); finished <- e }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("relay worker did not start")
		}
		if _, e := db.Exec("UPDATE save_batch_item SET claim_seq=claim_seq+1,lease_expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE batch_id=?", firstBatch); e != nil {
			close(resume)
			t.Fatal(e)
		}
		close(resume)
		if e := <-finished; e != nil {
			t.Fatal(e)
		}
		var grants int
		if e := db.QueryRow("SELECT COUNT(*) FROM media_object WHERE user_id=? AND asset_id=?", login.UserID, aid).Scan(&grants); e != nil || grants != 0 {
			t.Fatal("stale media worker published a grant", e)
		}
		if ok, e := live.preparePersistentItem(ctx, "relay-new"); e != nil || !ok {
			t.Fatal("media lease recovery", e)
		}
		b := request(lh, "GET", "/v1/save-batches/"+firstBatch, "", "", 200)
		if b["status"] != "READY" {
			t.Fatalf("relay did not become ready: %v", b)
		}
		itemID := b["items"].([]any)[0].(map[string]any)["itemId"].(string)
		ids, _ := json.Marshal(dto{"itemIds": []string{itemID}})
		tickets := request(lh, "POST", "/v1/save-batches/"+firstBatch+"/tickets", "", string(ids), 200)
		address := tickets["tickets"].([]any)[0].(map[string]any)["url"].(string)
		u, e := url.Parse(address)
		if e != nil {
			t.Fatal(e)
		}
		get := httptest.NewRequest("GET", u.RequestURI(), nil)
		get.Header.Set("Range", "bytes=0-11")
		reply := httptest.NewRecorder()
		lh.ServeHTTP(reply, get)
		if reply.Code != 206 || reply.Body.Len() != 12 || string(reply.Body.Bytes()[4:8]) != "ftyp" {
			t.Fatalf("relay did not serve validated file bytes: %d", reply.Code)
		}
		var charged int64
		if e = db.QueryRow("SELECT value FROM usage_counter WHERE subject_key=? AND kind='media_egress'", login.UserID).Scan(&charged); e != nil || charged != 12 {
			t.Fatal("Range transfer did not settle actual bytes", charged, e)
		}
		objectID := b["items"].([]any)[0].(map[string]any)["mediaObjectId"].(string)
		if _, e = db.Exec("UPDATE media_object SET status='revoked' WHERE id=?", objectID); e != nil {
			t.Fatal(e)
		}
		request(lh, "POST", "/v1/save-batches/"+firstBatch+"/tickets", "", string(ids), 410)
		if _, e = db.Exec("UPDATE media_object SET status='ready' WHERE id=?", objectID); e != nil {
			t.Fatal(e)
		}
		secondBatch := makeBatch()
		if ok, e := live.preparePersistentItem(ctx, "shared-worker"); e != nil || !ok {
			t.Fatal(e)
		}
		request(lh, "GET", "/v1/save-batches/"+secondBatch, "", "", 200)
		if downloads != 1 {
			t.Fatalf("same ready file fetched %d times", downloads)
		}
		other, e := sessions.DevLogin(ctx, auth.ID())
		if e != nil {
			t.Fatal(e)
		}
		defer cleanupUser(other.UserID)
		otherRequest := func(method, path, body string, want int) map[string]any {
			t.Helper()
			return requestAs(lh, other.Token, method, path, auth.ID(), body, want)
		}
		otherJob := otherRequest("POST", "/v1/parse-jobs", body, 202)["jobId"].(string)
		otherClaim, e := live.claimParse(ctx, "other-user-parser")
		if e != nil || otherClaim.Job.ID != otherJob {
			t.Fatal("could not claim second user's job", e)
		}
		if e = live.finishParse(ctx, otherClaim, parsed, nil); e != nil {
			t.Fatal(e)
		}
		otherResult := otherRequest("GET", "/v1/parse-jobs/"+otherJob, "", 200)["resultId"].(string)
		otherAsset := otherRequest("GET", "/v1/results/"+otherResult, "", 200)["assets"].([]any)[0].(map[string]any)["assetId"].(string)
		otherBody, _ := json.Marshal(dto{"resultId": otherResult, "assetIds": []string{otherAsset}, "clientActionId": auth.ID(), "flowId": "durable-test"})
		otherBatch := otherRequest("POST", "/v1/save-batches", string(otherBody), 202)["batchId"].(string)
		if ok, e := live.preparePersistentItem(ctx, "other-user-media"); e != nil || !ok {
			t.Fatal("cross-user sharing failed", e)
		}
		otherReady := otherRequest("GET", "/v1/save-batches/"+otherBatch, "", 200)
		otherItem := otherReady["items"].([]any)[0].(map[string]any)["itemId"].(string)
		otherIDs, _ := json.Marshal(dto{"itemIds": []string{otherItem}})
		otherURL := otherRequest("POST", "/v1/save-batches/"+otherBatch+"/tickets", string(otherIDs), 200)["tickets"].([]any)[0].(map[string]any)["url"].(string)
		var sharedFiles int
		if e = db.QueryRow("SELECT COUNT(DISTINCT file_id) FROM media_object WHERE user_id IN (?,?)", login.UserID, other.UserID).Scan(&sharedFiles); e != nil || sharedFiles != 1 || downloads != 1 {
			t.Fatal("users did not share one validated file", sharedFiles, downloads, e)
		}
		otherRequest("GET", "/v1/save-batches/"+firstBatch, "", 404)
		request(lh, "DELETE", "/v1/me/data", "", "", 200)
		denied := httptest.NewRecorder()
		lh.ServeHTTP(denied, httptest.NewRequest("GET", address, nil))
		if denied.Code != 403 {
			t.Fatal("deleted user retained media access", denied.Code)
		}
		surviving := httptest.NewRecorder()
		otherGet := httptest.NewRequest("GET", otherURL, nil)
		otherGet.Header.Set("Range", "bytes=0-11")
		lh.ServeHTTP(surviving, otherGet)
		if surviving.Code != 206 || surviving.Body.Len() != 12 {
			t.Fatal("deleting one user revoked the other user's file", surviving.Code)
		}
		var fileID, key string
		if e = db.QueryRow("SELECT f.id,f.storage_key FROM media_file f JOIN media_object o ON o.file_id=f.id WHERE o.user_id=? AND o.asset_id=? ORDER BY o.created_at DESC LIMIT 1", login.UserID, aid).Scan(&fileID, &key); e != nil {
			t.Fatal(e)
		}
		_, release, e := relay.Acquire(key)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("UPDATE media_object SET expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE file_id=?", fileID); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec("UPDATE media_file SET expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", fileID); e != nil {
			t.Fatal(e)
		}
		relay.CleanOnce(ctx)
		if f, e := relay.Open(key); e != nil {
			t.Fatal("GC removed an active transfer")
		} else {
			f.Close()
		}
		release()
		relay.CleanOnce(ctx)
		if f, e := relay.Open(key); e == nil {
			f.Close()
			t.Fatal("expired unreferenced file survived GC")
		}
	})
}

type durableMediaTransport func(*http.Request) (*http.Response, error)

func (f durableMediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
