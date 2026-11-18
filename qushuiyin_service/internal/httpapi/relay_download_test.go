package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"qushuiyin/service/internal/media"
)

func TestMediaRangeReservation(t *testing.T) {
	for _, tc := range []struct {
		header string
		bytes  int64
		valid  bool
	}{
		{"", 100, true},
		{"bytes=0-11", 12, true},
		{"bytes=90-", 10, true},
		{"bytes=-12", 12, true},
		{"bytes=-200", 100, true},
		{"bytes=90-200", 10, true},
		{"bytes=100-", 0, false},
		{"bytes=20-10", 0, false},
		{"bytes=-0", 0, false},
		{"bytes=0-1,4-5", 0, false},
		{"other=0-1", 0, false},
		{"bytes=9999999999999999999999-", 0, false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			n, valid := mediaRangeSize(tc.header, 100)
			if n != tc.bytes || valid != tc.valid {
				t.Fatalf("reservation = %d, %v; want %d, %v", n, valid, tc.bytes, tc.valid)
			}
		})
	}
}

func TestBatchStatusReturnsNullForUnknownSize(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	b := &batch{ID: "batch", User: "owner", ExpiresAt: expires, Items: []item{
		{ID: "item", Status: "READY", Mode: "direct", Asset: asset{Kind: "video"}, AvailableUntil: expires},
	}}
	s := &Mock{Relay: &media.Relay{}, batches: map[string]*batch{b.ID: b}}
	r := httptest.NewRequest("GET", "/v1/save-batches/"+b.ID, nil)
	r.SetPathValue("batchId", b.ID)
	w := httptest.NewRecorder()
	s.batchStatus(w, r, credential{User: b.User})
	var out struct {
		Data struct {
			Items []map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Data.Items) != 1 || string(out.Data.Items[0]["sizeBytes"]) != "null" {
		t.Fatalf("unknown size was not encoded as null: %s", w.Body.String())
	}
}
