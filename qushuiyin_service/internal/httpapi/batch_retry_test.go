package httpapi

import (
	"encoding/json"
	"testing"

	"qushuiyin/service/internal/auth"
)

func TestBatchRetryAfterServiceBusyMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.policy("relay_only")
	h.s.Relay.Settings.Data["media_cache_budget_bytes"] = json.RawMessage(`1`)
	b, id, failed := h.batch()
	issue, ok := failed["error"].(map[string]any)
	if !ok || issue["code"] != "SERVICE_BUSY" || issue["retryable"] != true {
		t.Fatal("expected retryable media preparation error", failed)
	}
	h.req("POST", "/v1/save-batches/"+b+"/retry", auth.ID(), dto{"itemIds": []string{id}}, 202)
	status := h.req("GET", "/v1/save-batches/"+b, "", nil, 200)
	retried := status["items"].([]any)[0].(map[string]any)
	if retried["prepareStatus"] != "PREPARING" || retried["attempt"] != float64(2) {
		t.Fatal("retry did not requeue the item", retried)
	}
}
