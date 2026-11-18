package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/wujunwei928/parse-video/parser"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/parse"
)

func TestMockParseJobsCancelBeforeResultAndKeepQueueMoving(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mockJobs(t)
		started, release := make(chan struct{}), make(chan struct{})
		releaseResolver := sync.OnceFunc(func() { close(release) })
		defer releaseResolver()
		const firstURL = "https://www.douyin.com/video/10001"
		s.Adapter.Resolve = func(ctx context.Context, raw string) (*parser.VideoParseInfo, error) {
			if raw == firstURL {
				close(started)
				<-release // Simulate a resolver returning success after cancellation.
			}
			return &parser.VideoParseInfo{VideoUrl: "https://media.example.test/v.mp4"}, nil
		}
		first := submitMockJob(t, s, "a", firstURL, false)
		<-started
		second := submitMockJob(t, s, "b", "https://www.douyin.com/video/10002", false)
		if got := readMockJob(t, s, "a", first.JobID, http.MethodGet); got.Status != "RUNNING" {
			t.Fatalf("expected running job: %+v", got)
		}
		if got := readMockJob(t, s, "b", second.JobID, http.MethodGet); got.Status != "QUEUED" {
			t.Fatalf("expected queued job while the worker is occupied: %+v", got)
		}
		readMockJob(t, s, "a", first.JobID, http.MethodDelete)
		releaseResolver()
		synctest.Wait()
		if got := readMockJob(t, s, "a", first.JobID, http.MethodGet); got.Status != "CANCELLED" || got.ResultID != "" {
			t.Fatalf("cancelled job published a result: %+v", got)
		}
		if got := readMockJob(t, s, "b", second.JobID, http.MethodGet); got.Status != "SUCCEEDED" || got.ResultID == "" {
			t.Fatalf("queued job could not complete: %+v", got)
		}
	})
}

func TestMockParseCacheSharesOnlyResultDataAndRefreshes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mockJobs(t)
		calls := 0
		s.Adapter.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
			calls++
			title := "original"
			if calls > 1 {
				title = "refreshed"
			}
			return &parser.VideoParseInfo{VideoUrl: "https://media.example.test/v.mp4", Title: title}, nil
		}
		const source = "https://www.douyin.com/video/10001?signature=a%2Bb"
		parseAs := func(user, raw string, force bool) *result {
			j := submitMockJob(t, s, user, raw, force)
			synctest.Wait()
			status := readMockJob(t, s, user, j.JobID, http.MethodGet)
			if status.Status != "SUCCEEDED" {
				t.Fatalf("parse failed: %+v", status)
			}
			return s.results[status.ResultID]
		}
		a := parseAs("a", source+"#share", false)
		b := parseAs("b", source, false)
		if calls != 1 || a.ID == b.ID || a.Assets[0].ID == b.Assets[0].ID || b.User != "b" {
			t.Fatal("cache must share data but allocate user-specific results and assets")
		}
		parseAs("a", source, true)
		if v := parseAs("b", source, false); calls != 2 || *v.Value.Title != "refreshed" {
			t.Fatal("forced refresh did not replace the shared result")
		}
		parseAs("b", "https://www.douyin.com/video/10001?signature=different", false)
		if calls != 3 {
			t.Fatal("distinct signature was lost from the cache key")
		}
	})
}

func mockJobs(t *testing.T) *Mock {
	t.Helper()
	var values map[string]json.RawMessage
	const settings = `{"parser_workers":1,"parse_cache_ttl_seconds":300,"max_request_body_bytes":4096,"max_share_text_chars":5000,"max_candidate_urls":4,"user_active_parse_jobs":2,"memory_parse_cache_max_entries":20,"memory_mock_max_entries":20,"memory_rate_limit_max_subjects":20,"user_parse_per_minute":10,"parse_job_timeout_seconds":25,"result_ttl_seconds":1800,"idempotency_ttl_seconds":3600}`
	if err := json.Unmarshal([]byte(settings), &values); err != nil {
		t.Fatal(err)
	}
	a, err := parse.Mock(config.Defaults(), parse.ValidationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	s := &Mock{Settings: config.Values{Data: values}, Adapter: a}
	s.Handler(t.Context())
	return s
}

type mockJobView struct{ JobID, Status, ResultID string }

func submitMockJob(t *testing.T, s *Mock, user, raw string, force bool) mockJobView {
	t.Helper()
	body, err := json.Marshal(jobRequest{ShareText: raw, Source: "manual", FlowID: "fixture-flow", ForceRefresh: force})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/parse-jobs", strings.NewReader(string(body)))
	req.Header.Set("Idempotency-Key", string(body))
	req.Header.Set("Idempotency-Key", requestID(httptest.NewRecorder()))
	w := httptest.NewRecorder()
	s.createJob(w, req, credential{User: user})
	return decodeMockJob(t, w, http.StatusAccepted)
}

func readMockJob(t *testing.T, s *Mock, user, id, method string) mockJobView {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/parse-jobs/"+id, nil)
	req.SetPathValue("jobId", id)
	w := httptest.NewRecorder()
	s.jobStatus(w, req, credential{User: user})
	return decodeMockJob(t, w, http.StatusOK)
}

func decodeMockJob(t *testing.T, w *httptest.ResponseRecorder, status int) mockJobView {
	t.Helper()
	var envelope struct{ Data mockJobView }
	if w.Code != status || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.JobID == "" {
		t.Fatalf("unexpected status or response: %d %s", w.Code, w.Body.String())
	}
	return envelope.Data
}
