package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"qushuiyin/service/internal/auth"
)

type bufferedResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(p)
}

type keyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	token chan struct{}
	users int
}

func (m *keyedMutex) lock(key string) func() {
	unlock, _ := m.lockContext(context.Background(), key)
	return unlock
}

func (m *keyedMutex) lockContext(ctx context.Context, key string) (func(), error) {
	m.mu.Lock()
	if m.entries == nil {
		m.entries = make(map[string]*keyedMutexEntry)
	}
	entry := m.entries[key]
	if entry == nil {
		entry = &keyedMutexEntry{token: make(chan struct{}, 1)}
		m.entries[key] = entry
	}
	entry.users++
	m.mu.Unlock()

	drop := func() {
		m.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(m.entries, key)
		}
		m.mu.Unlock()
	}
	select {
	case entry.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-entry.token
			drop()
			return nil, err
		}
		return func() {
			<-entry.token
			drop()
		}, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type batchRequestKey struct{}

type batchRequestState struct {
	batches   map[string]*batch
	keys      map[string]idem
	original  *batch
	replayKey string
}

func (s *Mock) batchState(r *http.Request) (batchRequestState, func()) {
	if state, ok := r.Context().Value(batchRequestKey{}).(batchRequestState); ok {
		return state, func() {}
	}
	s.mu.Lock()
	return batchRequestState{batches: s.batches, keys: s.keys}, s.mu.Unlock
}

func (s *Mock) pruneMockBatches(now time.Time) {
	if !s.Persistent {
		s.prune(now)
	}
}

func (s *Mock) batchURLRate(user string) bool {
	if s.Persistent {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return s.minute("ticket:"+user, s.Settings.Int("user_download_url_requests_per_minute"))
}

func (s *Mock) batchCopyRate(user string) bool {
	if s.Persistent {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	return s.rateUntil("copy:"+user, s.Settings.Int("user_copy_link_per_day"), dayStart(time.Now()).AddDate(0, 0, 1))
}

func (s *Mock) authorizedBatch(next authed) http.HandlerFunc {
	if !s.Persistent {
		return s.authorized(next)
	}
	return s.authorized(func(w http.ResponseWriter, r *http.Request, c credential) {
		s.persistentBatchRequest(w, r, c, next)
	})
}

func (s *Mock) persistentBatchRequest(w http.ResponseWriter, r *http.Request, c credential, next authed) {
	readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !readOnly {
		unlock, err := s.batchGates.lockContext(r.Context(), c.User)
		if err != nil {
			return
		}
		defer unlock()
	}
	state, err := s.loadBatchRequest(r, c.User, readOnly)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), batchRequestKey{}, state))
	if readOnly {
		next(w, r, c)
		return
	}
	out := &bufferedResponse{header: w.Header().Clone()}
	next(out, r, c)
	success := out.status < 400
	ticketExpired := out.status == http.StatusGone && r.Pattern == "POST /v1/save-batches/{batchId}/tickets"
	if success || ticketExpired {
		if err := s.saveBatchRequest(r.Context(), c, state, success); err != nil {
			if errors.Is(err, errCopyQuota) {
				fail(w, 429, "DAILY_LIMIT_REACHED")
				return
			}
			fail(w, 503, "SERVICE_BUSY")
			return
		}
	}
	for k, values := range out.header {
		w.Header()[k] = values
	}
	w.WriteHeader(out.status)
	_, _ = w.Write(out.body.Bytes())
}

func (s *Mock) loadBatchRequest(r *http.Request, user string, readOnly bool) (batchRequestState, error) {
	state := batchRequestState{batches: make(map[string]*batch), keys: make(map[string]idem)}
	ctx := r.Context()
	id := r.PathValue("batchId")
	var operation string
	switch r.Pattern {
	case "POST /v1/save-batches":
		operation = "prepare"
	case "POST /v1/save-batches/{batchId}/retry":
		operation = "batch-retry"
	case "POST /v1/save-batches/{batchId}/items/{itemId}/fallback":
		operation = "batch-fallback"
	}
	key := r.Header.Get("Idempotency-Key")
	if operation != "" && key != "" && len(key) <= 100 {
		var rec idem
		var response []byte
		err := s.Sessions.DB.QueryRowContext(ctx, "SELECT payload_hash,object_id,expires_at,response_json FROM idempotency_record WHERE user_id=? AND operation=? AND key_value=? AND expires_at>UTC_TIMESTAMP(3)", user, operation, key).Scan(&rec.Digest, &rec.ID, &rec.Expires, &response)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return state, err
		}
		if err == nil {
			if response != nil {
				if err = json.Unmarshal(response, &rec.Response); err != nil {
					return state, err
				}
			}
			state.replayKey = user + ":" + operation + ":" + key
			state.keys[state.replayKey] = rec
			if operation == "prepare" {
				id = rec.ID
			}
		}
	}
	if id == "" {
		return state, nil
	}
	b, err := s.loadBatch(ctx, user, id, !readOnly)
	if err == nil && b != nil {
		original := *b
		original.Items = slices.Clone(b.Items)
		state.original = &original
		state.batches[b.ID] = b
	}
	return state, err
}

func (s *Mock) loadBatch(ctx context.Context, user, id string, directURLs bool) (*batch, error) {
	b := &batch{ID: id, User: user}
	var enc sql.NullString
	var version uint64
	var expires time.Time
	err := s.Sessions.DB.QueryRowContext(ctx, "SELECT state_enc,version,expires_at FROM save_batch WHERE id=? AND user_id=?", id, user).Scan(&enc, &version, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if enc.Valid && time.Now().Before(expires) {
		if err = s.openTask(id, "batch", enc.String, b); err != nil {
			return nil, err
		}
	}
	b.Version, b.ExpiresAt = version, expires
	if !directURLs || len(b.Items) == 0 {
		return b, nil
	}
	rows, err := s.Sessions.DB.QueryContext(ctx, "SELECT id,direct_url_enc FROM save_batch_item WHERE batch_id=? AND user_id=? AND direct_url_enc IS NOT NULL AND direct_expires_at>UTC_TIMESTAMP(3)", id, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var itemID, urlEnc string
		if err = rows.Scan(&itemID, &urlEnc); err != nil {
			return nil, err
		}
		if x := b.findItem(itemID); x != nil {
			if err = s.openTask(itemID, "direct", urlEnc, &x.DirectURL); err != nil {
				return nil, err
			}
		}
	}
	return b, rows.Err()
}

func (s *Mock) batchResult(ctx context.Context, user, id string) (*result, error) {
	if !s.Persistent {
		return s.results[id], nil
	}
	res, err := s.loadPersistentResult(ctx, user, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res.result, nil
}

func (s *Mock) batchResultExpiry(ctx context.Context, user, id string) (time.Time, error) {
	var expires time.Time
	if !s.Persistent {
		if res := s.results[id]; res != nil && res.User == user {
			expires = res.ExpiresAt
		}
		return expires, nil
	}
	err := s.Sessions.DB.QueryRowContext(ctx, "SELECT expires_at FROM parse_result WHERE id=? AND user_id=?", id, user).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return expires, err
}

func (s *Mock) batchCapacity(ctx context.Context, user, action string, now time.Time) (int64, bool, error) {
	var active int64
	var duplicate bool
	if s.Persistent {
		err := s.Sessions.DB.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM save_batch WHERE user_id=? AND status='PREPARING' AND expires_at>?), EXISTS(SELECT 1 FROM save_batch WHERE user_id=? AND client_action_id=?)", user, now, user, action).Scan(&active, &duplicate)
		return active, duplicate, err
	}
	for _, b := range s.batches {
		if b.User == user && now.Before(b.ExpiresAt) {
			if b.Status == "PREPARING" {
				active++
			}
			duplicate = duplicate || b.ClientActionID == action
		}
	}
	return active, duplicate, nil
}

func (s *Mock) persistentEventOwner(e clientEvent, user string, now time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), s.Settings.Seconds("database_operation_timeout_seconds"))
	defer cancel()
	var resultID sql.NullString
	if e.JobID != nil {
		var flow string
		var expires time.Time
		err := s.Sessions.DB.QueryRowContext(ctx, "SELECT result_id,flow_id,expires_at FROM parse_job WHERE id=? AND user_id=?", *e.JobID, user).Scan(&resultID, &flow, &expires)
		if err != nil || !now.Before(expires) || e.FlowID != nil && *e.FlowID != flow {
			return false
		}
	}
	if e.BatchID == nil {
		return e.ItemID == nil
	}
	var rid, flow, action string
	var expires time.Time
	err := s.Sessions.DB.QueryRowContext(ctx, "SELECT result_id,flow_id,client_action_id,expires_at FROM save_batch WHERE id=? AND user_id=?", *e.BatchID, user).Scan(&rid, &flow, &action, &expires)
	if err != nil || !now.Before(expires) || resultID.Valid && resultID.String != "" && resultID.String != rid || e.FlowID != nil && *e.FlowID != flow || e.ClientActionID != nil && *e.ClientActionID != action {
		return false
	}
	if e.ItemID == nil {
		return true
	}
	var n int
	return s.Sessions.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM save_batch_item WHERE id=? AND batch_id=? AND user_id=?", *e.ItemID, *e.BatchID, user).Scan(&n) == nil && n == 1
}
func (s *Mock) saveBatchRequest(ctx context.Context, c credential, state batchRequestState, commitSignals bool) error {
	var b *batch
	for _, b = range state.batches {
	}
	if b == nil {
		return nil
	}
	original := state.original
	batchChanged := original == nil || b.Status != original.Status || b.Cancelled != original.Cancelled
	changedItems := make([]item, 0, len(b.Items))
	signals := make([]item, 0, len(b.Items))
	for i, x := range b.Items {
		if x.DirectIssuedPending+x.CopyPending+x.RelayIssuedPending > 0 || x.FallbackPending {
			if commitSignals {
				signals = append(signals, x)
			}
			x.DirectIssuedPending, x.CopyPending, x.RelayIssuedPending, x.FallbackPending = 0, 0, 0, false
		}
		if original == nil || x != original.Items[i] {
			changedItems = append(changedItems, x)
		}
	}
	batchChanged = batchChanged || len(changedItems) > 0
	newKey := len(state.keys) > 0 && state.replayKey == ""
	if !batchChanged && len(signals) == 0 && !newKey {
		return nil
	}

	windows, release := s.lockHostWindows(signals)
	defer release()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockUser(ctx, tx, c); err != nil {
		return err
	}
	if batchChanged {
		if err = s.writeBatch(ctx, tx, b, changedItems); err != nil {
			return err
		}
	}
	if newKey {
		for composite, rec := range state.keys {
			parts := strings.SplitN(composite, ":", 3)
			var response any
			if rec.Response != nil {
				raw, err := json.Marshal(rec.Response)
				if err != nil {
					return err
				}
				response = string(raw)
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO idempotency_record(id,user_id,operation,key_value,payload_hash,object_type,object_id,expires_at,response_json) VALUES (?,?,?,?,?,'save_batch',?,?,?) ON DUPLICATE KEY UPDATE payload_hash=IF(expires_at<=UTC_TIMESTAMP(3),VALUES(payload_hash),payload_hash),object_id=IF(expires_at<=UTC_TIMESTAMP(3),VALUES(object_id),object_id),response_json=IF(expires_at<=UTC_TIMESTAMP(3),VALUES(response_json),response_json),expires_at=IF(expires_at<=UTC_TIMESTAMP(3),VALUES(expires_at),expires_at)", auth.ID(), c.User, parts[1], parts[2], rec.Digest, rec.ID, rec.Expires, response)
			if err != nil {
				return err
			}
		}
	}
	for _, x := range signals {
		if err = s.commitDeliverySignals(ctx, tx, b, x, windows); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	deliveryLog(windows, s.hostWindows, s.Settings)
	for host, window := range windows {
		s.hostWindows[host] = window
	}
	s.mu.Unlock()
	return nil
}

func (s *Mock) writeBatch(ctx context.Context, tx *sql.Tx, b *batch, items []item) error {
	enc, err := s.sealTask(b.ID, "batch", b)
	if err != nil {
		return err
	}
	deadline := b.ExpiresAt
	for _, x := range b.Items {
		deadline = minTime(deadline, x.Deadline)
	}
	if b.Version == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO save_batch(id,user_id,result_id,flow_id,client_action_id,status,cancel_requested,deadline_at,expires_at,state_enc) VALUES (?,?,?,?,?,?,?,?,?,?)", b.ID, b.User, b.ResultID, b.FlowID, b.ClientActionID, b.Status, b.Cancelled, deadline, b.ExpiresAt, enc)
	} else {
		var changed sql.Result
		changed, err = tx.ExecContext(ctx, "UPDATE save_batch SET status=?,cancel_requested=?,deadline_at=?,state_enc=?,version=version+1 WHERE id=? AND user_id=? AND version=?", b.Status, b.Cancelled, deadline, enc, b.ID, b.User, b.Version)
		if err == nil {
			n, _ := changed.RowsAffected()
			if n != 1 {
				return errors.New("batch changed during operation")
			}
		}
	}
	if err != nil {
		return err
	}
	for _, x := range items {
		if b.Version > 0 {
			// A new attempt must fence the old worker before updating its state.
			_, err = tx.ExecContext(ctx, "UPDATE save_batch_item SET lease_owner=NULL,lease_expires_at=NULL,claim_seq=claim_seq+1 WHERE id=? AND user_id=? AND attempt<?", x.ID, b.User, x.Attempt)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO save_batch_item(id,user_id,batch_id,asset_id,order_no,prepare_status,delivery_mode,direct_fallback_used,copy_link_available,media_object_id,attempt,error_code) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE prepare_status=VALUES(prepare_status),delivery_mode=VALUES(delivery_mode),direct_fallback_used=VALUES(direct_fallback_used),copy_link_available=VALUES(copy_link_available),media_object_id=VALUES(media_object_id),attempt=VALUES(attempt),error_code=VALUES(error_code),version=version+1", x.ID, b.User, b.ID, x.Asset.ID, x.Asset.Order, x.Status, x.Mode, x.Fallback, x.CopyAvailable, x.ObjectID, x.Attempt, x.Failure)
		if err != nil {
			return err
		}
		if err = s.saveDirect(ctx, tx, b.User, x); err != nil {
			return err
		}
	}
	return nil
}

// A batch item is a durable work unit. A worker only publishes while its
// sequence and lease still match, so a late process cannot replace new work.
func (s *Mock) RunPersistentMedia(ctx context.Context) {
	for i := int64(0); i < s.Settings.Int("media_workers"); i++ {
		go s.mediaWorker(ctx, auth.ID())
	}
}
func (s *Mock) mediaWorker(ctx context.Context, owner string) {
	interval := time.Duration(s.Settings.Int("poll_interval_ms")) * time.Millisecond
	for ctx.Err() == nil {
		worked, err := s.preparePersistentItem(ctx, owner)
		if worked && err == nil {
			continue
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}
func (s *Mock) preparePersistentItem(ctx context.Context, owner string) (bool, error) {
	release, err := s.acquireMediaWork(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var bid, iid, user, enc string
	var seq uint64
	err = tx.QueryRowContext(ctx, `SELECT i.id,i.batch_id,i.user_id,i.claim_seq,b.state_enc FROM save_batch_item i JOIN save_batch b ON b.id=i.batch_id JOIN parse_result p ON p.id=b.result_id JOIN app_user u ON u.id=i.user_id WHERE u.status='active' AND p.adapter_version=? AND b.cancel_requested=FALSE AND b.expires_at>UTC_TIMESTAMP(3) AND i.prepare_status IN ('PENDING','PREPARING') AND (i.lease_expires_at IS NULL OR i.lease_expires_at<=UTC_TIMESTAMP(3)) ORDER BY i.created_at,i.id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.adapterVersion()).Scan(&iid, &bid, &user, &seq, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b := &batch{}
	if err = s.openTask(bid, "batch", enc, b); err != nil {
		return false, err
	}
	x := b.findItem(iid)
	if x == nil {
		return false, errors.New("missing batch item")
	}
	deadline := minTime(x.Deadline, b.ExpiresAt)
	lease := s.Settings.Seconds("media_fetch_timeout_seconds")
	leaseExpires := minTime(deadline, time.Now().Add(lease)).Truncate(time.Millisecond)
	seq++
	_, err = tx.ExecContext(ctx, "UPDATE save_batch_item SET lease_owner=?,lease_expires_at=?,claim_seq=?,prepare_status='PREPARING',version=version+1 WHERE id=?", owner, leaseExpires, seq, iid)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	if !time.Now().Before(deadline) {
		return true, s.publishPersistentItem(ctx, owner, seq, b, *x)
	}
	call, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	retry := time.Duration(s.Settings.Int("poll_interval_ms")) * time.Millisecond
	go func() {
		if !keepMediaLease(call, leaseExpires, deadline, lease, retry, func(ctx context.Context, next time.Time) (int64, error) {
			result, err := s.Sessions.DB.ExecContext(ctx, `UPDATE save_batch_item SET lease_expires_at=?,version=version+1
				WHERE id=? AND user_id=? AND claim_seq=? AND lease_owner=? AND prepare_status='PREPARING' AND lease_expires_at>UTC_TIMESTAMP(3)`, next, iid, user, seq, owner)
			if err != nil {
				return 0, err
			}
			return result.RowsAffected()
		}) {
			cancel()
		}
	}()
	res, err := s.loadPersistentResult(call, user, b.ResultID)
	if err == nil {
		if s.Relay == nil {
			local := Mock{Settings: s.Settings, results: map[string]*result{res.ID: res.result}}
			local.prepare(b, x, time.Now().UTC())
		} else {
			s.prepareDelivery(call, b, x, res)
		}
	}
	if call.Err() == context.Canceled || err != nil && call.Err() != context.DeadlineExceeded {
		return true, err
	}
	return true, s.publishPersistentItem(ctx, owner, seq, b, *x)
}

func (s *Mock) acquireMediaWork(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slots := s.mediaWork
	select {
	case slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-slots
			return nil, err
		}
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Keep the last confirmed expiry through transient database errors. A zero-row
// update proves the claim is lost, regardless of the local expiry.
func keepMediaLease(ctx context.Context, expires, deadline time.Time, lease, poll time.Duration, renew func(context.Context, time.Time) (int64, error)) bool {
	retry := min(poll, lease/4)
	wake := expires.Add(-lease / 2)
	for {
		if expires.Equal(deadline.Truncate(time.Millisecond)) {
			return true // The preparation context already owns the last deadline.
		}
		timer := time.NewTimer(time.Until(minTime(wake, expires)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
		now := time.Now()
		if !now.Before(expires) {
			return false
		}
		next := minTime(deadline, now.Add(lease)).Truncate(time.Millisecond)
		// Leave time for another attempt even if this query stalls.
		attempt, cancel := context.WithTimeout(ctx, min(lease/4, expires.Sub(now)/2))
		changed, err := renew(attempt, next)
		cancel()
		if err != nil {
			wake = time.Now().Add(retry)
			continue
		}
		if changed == 0 {
			return false
		}
		expires, wake = next, next.Add(-lease/2)
	}
}

func (s *Mock) publishPersistentItem(ctx context.Context, owner string, seq uint64, b *batch, x item) error {
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", b.User).Scan(&state); err != nil {
		return err
	}
	if state != "active" {
		return nil
	}
	var enc string
	if err = tx.QueryRowContext(ctx, "SELECT state_enc FROM save_batch WHERE id=? AND user_id=? FOR UPDATE", b.ID, b.User).Scan(&enc); err != nil {
		return err
	}
	latest := &batch{}
	if err = s.openTask(b.ID, "batch", enc, latest); err != nil {
		return err
	}
	if latest.Cancelled || !time.Now().Before(latest.ExpiresAt) {
		return nil
	}
	old := latest.findItem(x.ID)
	if old == nil || old.Attempt != x.Attempt || old.Status != "PREPARING" && old.Status != "PENDING" {
		return nil
	}
	timedOut := !time.Now().Before(old.Deadline)
	if timedOut {
		x = *old
		x.Status, x.Failure = "FAILED", "PREPARE_TIMEOUT"
	}
	changed, err := tx.ExecContext(ctx, "UPDATE save_batch_item SET prepare_status=?,delivery_mode=?,media_object_id=?,error_code=?,lease_owner=NULL,lease_expires_at=NULL,version=version+1 WHERE id=? AND user_id=? AND claim_seq=? AND lease_owner=? AND (lease_expires_at>UTC_TIMESTAMP(3) OR ?) AND prepare_status='PREPARING'", x.Status, x.Mode, x.ObjectID, x.Failure, x.ID, b.User, seq, owner, timedOut)
	if err != nil {
		return err
	}
	n, _ := changed.RowsAffected()
	if n != 1 {
		return nil
	}
	if err = s.saveDirect(ctx, tx, b.User, x); err != nil {
		return err
	}
	if x.Status == "READY" && x.FileID != "" {
		var fileState string
		var expires time.Time
		if err = tx.QueryRowContext(ctx, "SELECT status,expires_at FROM media_file WHERE id=? FOR UPDATE", x.FileID).Scan(&fileState, &expires); err != nil {
			return err
		}
		if fileState != "ready" || !time.Now().Before(expires) {
			return nil
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO media_object(id,user_id,asset_id,file_id,output_version,status,expires_at) VALUES (?,?,?,?,?,'ready',?)", x.ObjectID, b.User, x.Asset.ID, x.FileID, x.ObjectID, x.AvailableUntil)
		if err != nil {
			return err
		}
	}
	*old = x
	latest.aggregate()
	enc, err = s.sealTask(latest.ID, "batch", latest)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE save_batch SET status=?,state_enc=?,version=version+1 WHERE id=? AND user_id=?", latest.Status, enc, latest.ID, latest.User)
	if err != nil {
		return err
	}
	return tx.Commit()
}
