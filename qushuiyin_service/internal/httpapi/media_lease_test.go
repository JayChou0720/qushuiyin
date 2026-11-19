package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"qushuiyin/service/internal/auth"
)

func TestBatchLockWaitStopsWhenRequestDisconnects(t *testing.T) {
	s := &Mock{Persistent: true}
	unlock := s.batchGates.lock("owner")
	defer unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/save-batches", nil)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.persistentBatchRequest(httptest.NewRecorder(), r, credential{User: "owner"}, func(http.ResponseWriter, *http.Request, credential) {
			t.Error("disconnected request reached the endpoint")
		})
	}()
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request remained behind the user lock")
	}
}

func TestMediaLeaseRetriesDatabaseErrorsUntilClaimIsLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	now := time.Now()
	attempts := 0
	kept := keepMediaLease(ctx, now.Add(200*time.Millisecond), now.Add(time.Minute), 400*time.Millisecond, time.Millisecond, func(ctx context.Context, _ time.Time) (int64, error) {
		attempts++
		if attempts == 1 {
			return 0, errors.New("temporary database failure")
		}
		return 0, nil
	})
	if kept || attempts != 2 {
		t.Fatalf("wanted one retry before accepting the lost claim, got %v after %d attempts", kept, attempts)
	}
}

func TestPersistentMediaRenewsLeaseMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.register("registered")
	h.s.Relay.Settings.Data = maps.Clone(h.s.Settings.Data)
	h.s.Settings.Data["media_fetch_timeout_seconds"] = json.RawMessage(`2`)
	id := h.req("POST", "/v1/save-batches", auth.ID(), dto{
		"resultId": h.rid, "assetIds": []string{h.aid}, "flowId": "lease-test", "clientActionId": auth.ID(),
	}, 202)["batchId"].(string)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	transport := h.s.Relay.Client.Transport
	h.s.Relay.Client.Transport = durableMediaTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodHead && r.URL.Host == h.host {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return transport.RoundTrip(r)
	})
	finished := make(chan error, 1)
	go func() {
		_, err := h.s.preparePersistentItem(ctx, "lease-owner")
		finished <- err
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("worker stopped before probing: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	const query = `SELECT i.lease_expires_at,b.deadline_at,i.claim_seq FROM save_batch_item i JOIN save_batch b ON b.id=i.batch_id WHERE b.id=? AND b.user_id=?`
	var initial, deadline time.Time
	var seq uint64
	if err := h.db.QueryRowContext(ctx, query, id, h.user).Scan(&initial, &deadline, &seq); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(initial) + 50*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var renewed time.Time
	var currentSeq uint64
	if err := h.db.QueryRowContext(ctx, query, id, h.user).Scan(&renewed, &deadline, &currentSeq); err != nil {
		t.Fatal(err)
	}
	if !renewed.After(time.Now()) || renewed.After(deadline) || currentSeq != seq {
		t.Fatalf("invalid renewed lease %v at deadline %v, claim %d -> %d", renewed, deadline, seq, currentSeq)
	}
	if worked, err := h.s.preparePersistentItem(ctx, "competing-owner"); err != nil || worked {
		t.Fatalf("a second worker claimed the renewed item: %v, %v", worked, err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if data := h.req("GET", "/v1/save-batches/"+id, "", nil, 200); data["status"] != "READY" {
		t.Fatal("the original owner could not publish", data)
	}
}
