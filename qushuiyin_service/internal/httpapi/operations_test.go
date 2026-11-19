package httpapi

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"qushuiyin/service/internal/auth"
)

func TestOperationalCleanupMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	job := h.req("POST", "/v1/parse-jobs", auth.ID(), dto{
		"shareText": "https://www.douyin.com/video/10001", "source": "manual", "flowId": "cleanup-test",
	}, 202)["jobId"].(string)
	now := time.Now().UTC()
	if _, err := h.db.Exec("UPDATE parse_job SET deadline_at=? WHERE id=?", now.Add(-time.Second), job); err != nil {
		t.Fatal(err)
	}
	h.s.cleanDatabase(t.Context(), now)
	ended := h.req("GET", "/v1/parse-jobs/"+job, "", nil, 200)
	if ended["status"] != "FAILED" || ended["error"].(map[string]any)["code"] != "PARSE_TIMEOUT" {
		t.Fatal("cleanup did not finish the abandoned task")
	}
	cutoff := now.AddDate(0, 0, -int(h.s.settings().Int("tombstone_retention_days"))-1)
	if _, err := h.db.Exec("UPDATE parse_job SET expires_at=? WHERE id=?", cutoff, job); err != nil {
		t.Fatal(err)
	}
	h.s.cleanDatabase(t.Context(), now)
	h.req("GET", "/v1/parse-jobs/"+job, "", nil, 404)
	h.req("GET", "/v1/results/"+h.rid, "", nil, 200)

	limit := h.s.settings().Int("user_media_egress_per_day_bytes")
	_, _, err := h.s.reserveEgress(t.Context(), h.user, limit+1, false)
	if !errors.Is(err, errEgressQuota) {
		t.Fatalf("expected a daily egress limit, got %v", err)
	}
	w := httptest.NewRecorder()
	rejectMediaBudget(w, err)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("daily quota response omitted its reset time")
	}
}
