package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"
	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
	store "qushuiyin/service/internal/db"
	"qushuiyin/service/internal/media"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"qushuiyin/service/migrations"
	fixture "qushuiyin/service/testdata/media"
)

type deliveryHarness struct {
	t                           *testing.T
	db                          *sql.DB
	s                           *Mock
	h                           http.Handler
	user, token, rid, aid, host string
	size                        int64
	gets                        int
}

func newDeliveryHarness(t *testing.T) *deliveryHarness {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not configured")
	}
	c, e := mysql.ParseDSN(dsn)
	if e != nil || c.DBName != "qushuiyin_test" || c.Net != "tcp" || !strings.HasPrefix(c.Addr, "127.0.0.1:") {
		t.Fatal("isolated loopback test database required")
	}
	c.ParseTime = true
	c.Loc = time.UTC
	db, e := sql.Open("mysql", c.FormatDSN())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	p, e := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = p.Up(t.Context()); e != nil {
		t.Fatal(e)
	}
	v, e := config.Load(t.Context(), db)
	if e != nil {
		t.Fatal(e)
	}
	a, e := parse.Mock(config.Defaults(), parse.ValidationPolicy())
	if e != nil {
		t.Fatal(e)
	}
	s := &Mock{Persistent: true, Settings: v, Sessions: auth.Store{DB: db, Key: []byte(auth.ID()), TTL: time.Hour}, Adapter: a, Origin: "http://127.0.0.1:18080", TicketKey: []byte(auth.ID())}
	if e = s.ConfigureTaskKeys("v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); e != nil {
		t.Fatal(e)
	}
	login, e := s.Sessions.DevLogin(t.Context(), auth.ID())
	if e != nil {
		t.Fatal(e)
	}
	h := &deliveryHarness{t: t, db: db, s: s, user: login.UserID, token: login.Token, host: auth.ID() + ".cdn.test"}
	h.h = s.Handler(t.Context())
	var status string
	var caps []byte
	if e = db.QueryRow("SELECT status,capabilities FROM platform_config WHERE platform='douyin'").Scan(&status, &caps); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Exec("UPDATE platform_config SET status=?,capabilities=? WHERE platform='douyin'", status, caps)
		rows, _ := db.Query("SELECT DISTINCT file_id FROM media_object WHERE user_id=?", h.user)
		var ids []string
		if rows != nil {
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
		}
		for _, q := range cleanupUserQueries {
			if _, e := db.Exec(q, h.user); e != nil {
				t.Error(e)
			}
		}
		db.Exec("DELETE FROM cleanup_job WHERE user_id=?", h.user)
		db.Exec("DELETE FROM app_user WHERE id=?", h.user)
		db.Exec("DELETE FROM download_domain WHERE host=?", h.host)
		for _, id := range ids {
			db.Exec("DELETE FROM media_file WHERE id=?", id)
		}
	})
	h.policy("direct_preferred")
	_, body := fixture.File("video")
	h.size = int64(len(body))
	s.Relay = &media.Relay{DB: db, Domains: store.Domains{DB: db}, Directory: t.TempDir(), FFprobe: os.Getenv("FFPROBE_TEST_PATH"), Settings: v, Key: []byte(auth.ID()), Client: outbound.Client{Redirects: 5, Transport: durableMediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != h.host {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://" + h.host + "/video?signature=private-test"}}, Body: http.NoBody}, nil
		}
		if r.Method == "HEAD" {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/mp4"}}, ContentLength: h.size, Body: http.NoBody}, nil
		}
		h.gets++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"video/mp4"}}, ContentLength: int64(len(body)), Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
	created := h.req("POST", "/v1/parse-jobs", auth.ID(), dto{"shareText": "https://www.douyin.com/video/10001", "source": "manual", "flowId": "route-test"}, 202)
	j, e := s.claimParse(t.Context(), "test-parser")
	if e != nil || j.Job.ID != created["jobId"] {
		t.Fatal("claim parse", e)
	}
	r, e := a.Parse(t.Context(), j.Source)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.finishParse(t.Context(), j, r, nil); e != nil {
		t.Fatal(e)
	}
	h.rid = h.req("GET", "/v1/parse-jobs/"+j.Job.ID, "", nil, 200)["resultId"].(string)
	h.aid = h.req("GET", "/v1/results/"+h.rid, "", nil, 200)["assets"].([]any)[0].(map[string]any)["assetId"].(string)
	return h
}
func (h *deliveryHarness) policy(policy string) {
	h.t.Helper()
	caps, _ := json.Marshal(dto{"videoEnabled": true, "imageEnabled": true, "coverEnabled": true, "textEnabled": true, "deliveryPolicy": dto{"video": policy, "image": policy, "cover": policy}})
	if _, e := h.db.Exec("UPDATE platform_config SET status='enabled',capabilities=? WHERE platform='douyin'", caps); e != nil {
		h.t.Fatal(e)
	}
}
func (h *deliveryHarness) req(method, path, key string, body any, want int) map[string]any {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	if w.Code != want {
		h.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var out struct{ Data map[string]any }
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		h.t.Fatal("bad JSON")
	}
	return out.Data
}
func (h *deliveryHarness) batch() (string, string, map[string]any) {
	h.t.Helper()
	b := h.req("POST", "/v1/save-batches", auth.ID(), dto{"resultId": h.rid, "assetIds": []string{h.aid}, "flowId": "route-test", "clientActionId": auth.ID()}, 202)["batchId"].(string)
	if ok, e := h.s.preparePersistentItem(h.t.Context(), auth.ID()); e != nil || !ok {
		h.t.Fatal("prepare", e)
	}
	v := h.req("GET", "/v1/save-batches/"+b, "", nil, 200)
	x := v["items"].([]any)[0].(map[string]any)
	return b, x["itemId"].(string), x
}
func (h *deliveryHarness) register(state string) {
	h.t.Helper()
	_, e := h.s.Relay.Domains.Observe(h.t.Context(), "douyin", h.host)
	if e == nil {
		_, e = h.db.Exec("UPDATE download_domain SET status=? WHERE host=?", state, h.host)
	}
	if e != nil {
		h.t.Fatal(e)
	}
}

func TestDeliveryDirectFallbackMySQL(t *testing.T) {
	if os.Getenv("FFPROBE_TEST_PATH") == "" {
		t.Skip("FFPROBE_TEST_PATH not configured")
	}
	h := newDeliveryHarness(t)
	h.register("registered")
	b, i, x := h.batch()
	if x["deliveryMode"] != "direct" || x["prepareStatus"] != "READY" || h.gets != 0 {
		t.Fatal("direct preparation fetched a full file", x, h.gets)
	}
	tickets := h.req("POST", "/v1/save-batches/"+b+"/tickets", "", dto{"itemIds": []string{i}}, 200)
	address := tickets["tickets"].([]any)[0].(map[string]any)["url"].(string)
	if address != "https://"+h.host+"/video?signature=private-test" {
		t.Fatal("did not issue the final address")
	}
	var enc string
	if e := h.db.QueryRow("SELECT direct_url_enc FROM save_batch_item WHERE id=?", i).Scan(&enc); e != nil || strings.Contains(enc, "private-test") {
		t.Fatal("direct address not encrypted", e)
	}
	if loaded, e := h.s.loadBatch(t.Context(), h.user, b, true); e != nil || loaded == nil || loaded.findItem(i).DirectURL != address {
		t.Fatal("restart lost encrypted direct address", e)
	}
	path := "/v1/save-batches/" + b + "/items/" + i + "/fallback"
	key := auth.ID()
	reason := dto{"reason": "DIRECT_CONTENT_INVALID"}
	h.req("POST", path, auth.ID(), dto{"reason": "INVALID"}, 400)
	first := h.req("POST", path, key, reason, 202)
	h.req("POST", path, key, reason, 202)
	h.req("POST", path, auth.ID(), reason, 409)
	if ok, e := h.s.preparePersistentItem(t.Context(), "fallback-worker"); e != nil || !ok {
		t.Fatal(e)
	}
	x = h.req("GET", "/v1/save-batches/"+b, "", nil, 200)["items"].([]any)[0].(map[string]any)
	if x["deliveryMode"] != "relay" || h.gets != 1 {
		t.Fatal("fallback did not prepare a real relay file", x, h.gets)
	}
	h.s.Settings.Data["poll_interval_ms"] = json.RawMessage(`1234`)
	if replay := h.req("POST", path, key, reason, 202); !reflect.DeepEqual(replay, first) {
		t.Fatal("fallback replay changed the original acceptance", replay, first)
	}
	var issued, falls int
	if e := h.db.QueryRow("SELECT direct_issued_total,direct_fallback_total FROM download_domain WHERE host=?", h.host).Scan(&issued, &falls); e != nil || issued != 1 || falls != 1 {
		t.Fatal("idempotent fallback counted more than once", issued, falls, e)
	}
}

func TestTicketCountingToleratesAnotherItemPublicationMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.register("registered")
	b, itemID, _ := h.batch()
	r := httptest.NewRequest(http.MethodPost, "/v1/save-batches/"+b+"/tickets", strings.NewReader(`{"itemIds":["`+itemID+`"]}`))
	r.Pattern = "POST /v1/save-batches/{batchId}/tickets"
	r.SetPathValue("batchId", b)
	w := httptest.NewRecorder()
	c := credential{User: h.user, Digest: h.s.Sessions.TokenDigest(h.token)}
	h.s.persistentBatchRequest(w, r, c, func(w http.ResponseWriter, r *http.Request, c credential) {
		h.s.tickets(w, r, c)
		if _, err := h.db.Exec("UPDATE save_batch SET version=version+1 WHERE id=? AND user_id=?", b, h.user); err != nil {
			t.Fatal(err)
		}
	})
	if w.Code != http.StatusOK {
		t.Fatalf("unrelated batch revision prevented ticket issuance: %d", w.Code)
	}
	var issued int
	if err := h.db.QueryRow("SELECT direct_issued_total FROM download_domain WHERE host=?", h.host).Scan(&issued); err != nil || issued != 1 {
		t.Fatalf("ticket issuance was not counted: %d, %v", issued, err)
	}
}

func TestDeliveryUnregisteredCopyAndBudgetsMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.size = h.s.Settings.Int("relay_max_video_bytes") + 1
	b, i, x := h.batch()
	if x["prepareStatus"] != "FAILED" || x["copyLinkAvailable"] != true || h.gets != 0 {
		t.Fatal("large unregistered media must offer only copy", x)
	}
	var state string
	var attempts int
	if e := h.db.QueryRow("SELECT status FROM download_domain WHERE host=?", h.host).Scan(&state); e != nil || state != "observed" {
		t.Fatal(state, e)
	}
	if e := h.db.QueryRow("SELECT direct_reparse_attempt FROM save_batch WHERE id=?", b).Scan(&attempts); e != nil || int64(attempts) != h.s.Settings.Int("direct_reparse_attempts") {
		t.Fatal("batch reparse budget", attempts, e)
	}
	copyPath := "/v1/save-batches/" + b + "/items/" + i + "/copy-link"
	if v := h.req("POST", copyPath, "", nil, 200); !strings.HasPrefix(v["url"].(string), "https://"+h.host+"/") {
		t.Fatal("wrong copy outlet")
	}
	h.s.Settings.Data["user_copy_link_per_day"] = json.RawMessage("1")
	h.req("POST", copyPath, "", nil, 429)
	oldToken := h.token
	h.token = "invalid"
	h.req("POST", copyPath, "", nil, 401)
	h.token = oldToken
	other, e := h.s.Sessions.DevLogin(t.Context(), auth.ID())
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		for _, q := range cleanupUserQueries {
			h.db.Exec(q, other.UserID)
		}
		h.db.Exec("DELETE FROM app_user WHERE id=?", other.UserID)
	}()
	h.token = other.Token
	h.req("POST", copyPath, "", nil, 404)
	h.req("POST", "/v1/save-batches/"+b+"/tickets", "", dto{"itemIds": []string{i}}, 404)
	h.token = oldToken
	if _, e = h.db.Exec("UPDATE save_batch_item SET direct_expires_at=UTC_TIMESTAMP(3)-INTERVAL 1 SECOND WHERE id=?", i); e != nil {
		t.Fatal(e)
	}
	h.req("POST", copyPath, "", nil, 410)
}

func TestSameResourcesRejectsImageReorder(t *testing.T) {
	id := "same-work"
	a := parse.Asset{Kind: "image", Role: "content", Order: 1, Source: "https://cdn.test/one.jpg"}
	b := a
	b.Order = 2
	b.Source = "https://cdn.test/two.jpg"
	old := &parse.Result{Platform: "douyin", ContentID: &id, MediaType: "image", Assets: []parse.Asset{a, b}}
	fresh := *old
	fresh.Assets = []parse.Asset{b, a}
	if sameResources(old, &fresh) {
		t.Fatal("reordered images accepted")
	}
	fresh.Assets = []parse.Asset{a, b}
	fresh.Assets[0].Source = b.Source
	fresh.Assets[1].Source = a.Source
	if sameResources(old, &fresh) {
		t.Fatal("changed image identity accepted at original order numbers")
	}
}

func TestDeliveryHostReviewAndCopyPolicyMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.register("registered")
	h.size = h.s.Settings.Int("relay_max_video_bytes") + 1
	h.s.Settings.Data["direct_host_review_min_failures"] = json.RawMessage("2")
	for n := 1; n <= 2; n++ {
		b, i, x := h.batch()
		if x["deliveryMode"] != "direct" {
			t.Fatal(x)
		}
		h.req("POST", "/v1/save-batches/"+b+"/tickets", "", dto{"itemIds": []string{i}}, 200)
		h.req("POST", "/v1/save-batches/"+b+"/items/"+i+"/fallback", auth.ID(), dto{"reason": "DIRECT_DOWNLOAD_REJECTED"}, 202)
		if ok, e := h.s.preparePersistentItem(t.Context(), auth.ID()); e != nil || !ok {
			t.Fatal(e)
		}
		x = h.req("GET", "/v1/save-batches/"+b, "", nil, 200)["items"].([]any)[0].(map[string]any)
		if x["copyLinkAvailable"] != true {
			t.Fatal("fallback should offer copy for a large file", x)
		}
		var state string
		if e := h.db.QueryRow("SELECT status FROM download_domain WHERE host=?", h.host).Scan(&state); e != nil {
			t.Fatal(e)
		}
		want := "registered"
		if n == 2 {
			want = "review"
		}
		if state != want {
			t.Fatal("host review threshold", state, want)
		}
	}
	h.policy("copy_only")
	b, _, x := h.batch()
	if x["prepareStatus"] != "FAILED" || x["copyLinkAvailable"] != true {
		t.Fatal("copy-only policy", x)
	}
	var n int
	h.db.QueryRow("SELECT direct_reparse_attempt FROM save_batch WHERE id=?", b).Scan(&n)
	if n != 0 {
		t.Fatal("copy-only reparsed")
	}
}
