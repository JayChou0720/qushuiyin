package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
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

func (s *Mock) persistentBatchRequest(w http.ResponseWriter, r *http.Request, c credential, next authed) {
	s.persistentGate.Lock()
	defer s.persistentGate.Unlock()
	// Reload authoritative rows before each operation. A worker publication is
	// checked by version when the operation commits.
	if err := s.loadBatches(r.Context(), c.User); err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	out := &bufferedResponse{header: make(http.Header)}
	next(out, r, c)
	if out.status < 400 {
		if err := s.saveBatches(r.Context(), c); err != nil {
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
func (s *Mock) loadBatches(ctx context.Context, user string) error {
	rows, err := s.Sessions.DB.QueryContext(ctx, "SELECT id,state_enc,version,expires_at FROM save_batch WHERE user_id=? AND (state_enc IS NOT NULL OR expires_at<=UTC_TIMESTAMP(3)) ORDER BY created_at DESC LIMIT ?", user, s.Settings.Int("memory_parse_cache_max_entries"))
	if err != nil {
		return err
	}
	loaded := map[string]*batch{}
	for rows.Next() {
		var id string
		var enc sql.NullString
		var expires time.Time
		var v uint64
		if err = rows.Scan(&id, &enc, &v, &expires); err != nil {
			rows.Close()
			return err
		}
		b := &batch{}
		if enc.Valid {
			if err = s.openTask(id, "batch", enc.String, b); err != nil {
				rows.Close()
				return err
			}
		} else {
			b.ID, b.User, b.ExpiresAt = id, user, expires
		}
		b.Version = v
		loaded[id] = b
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, b := range loaded {
		for i := range b.Items {
			x := &b.Items[i]
			var enc sql.NullString
			var expires sql.NullTime
			if err = s.Sessions.DB.QueryRowContext(ctx, "SELECT direct_url_enc,direct_expires_at FROM save_batch_item WHERE id=? AND user_id=?", x.ID, user).Scan(&enc, &expires); err != nil {
				return err
			}
			if enc.Valid && expires.Valid && time.Now().Before(expires.Time) {
				if err = s.openTask(x.ID, "direct", enc.String, &x.DirectURL); err != nil {
					return err
				}
			}
		}
	}
	results := map[string]*result{}
	for _, b := range loaded {
		res, e := s.loadPersistentResult(ctx, user, b.ResultID)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return e
		}
		results[res.ID] = res.result
	}
	rows, err = s.Sessions.DB.QueryContext(ctx, "SELECT operation,key_value,payload_hash,object_id,expires_at FROM idempotency_record WHERE user_id=? AND operation IN ('prepare','batch-retry','batch-fallback')", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	keys := map[string]idem{}
	for rows.Next() {
		var op, k string
		var rec idem
		if err = rows.Scan(&op, &k, &rec.Digest, &rec.ID, &rec.Expires); err != nil {
			return err
		}
		keys[user+":"+op+":"+k] = rec
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, b := range s.batches {
		if b.User == user {
			delete(s.batches, id)
		}
	}
	for k := range s.keys {
		if strings.HasPrefix(k, user+":") {
			delete(s.keys, k)
		}
	}
	for id, b := range loaded {
		s.batches[id] = b
	}
	for id, res := range results {
		s.results[id] = res
	}
	for k, v := range keys {
		s.keys[k] = v
	}
	return nil
}

func (s *Mock) persistentEventOwner(e clientEvent, user string, now time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), s.Settings.Seconds("parse_job_timeout_seconds"))
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
func (s *Mock) saveBatches(ctx context.Context, c credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockUser(ctx, tx, c); err != nil {
		return err
	}
	windows := map[string]hostWindow{}
	for k, v := range s.hostWindows {
		windows[k] = v
	}
	for _, b := range s.batches {
		if b.User != c.User || !time.Now().Before(b.ExpiresAt) {
			continue
		}
		enc, e := s.sealTask(b.ID, "batch", b)
		if e != nil {
			return e
		}
		deadline := b.ExpiresAt
		for _, x := range b.Items {
			// A retry is a new attempt and must not inherit the old worker's lease.
			_, err = tx.ExecContext(ctx, "UPDATE save_batch_item SET lease_owner=NULL,lease_expires_at=NULL,claim_seq=claim_seq+1 WHERE id=? AND user_id=? AND attempt<?", x.ID, b.User, x.Attempt)
			if err != nil {
				return err
			}
			deadline = minTime(deadline, x.Deadline)
		}
		if b.Version == 0 {
			_, err = tx.ExecContext(ctx, "INSERT INTO save_batch(id,user_id,result_id,flow_id,client_action_id,status,cancel_requested,deadline_at,expires_at,state_enc) VALUES (?,?,?,?,?,?,?,?,?,?)", b.ID, b.User, b.ResultID, b.FlowID, b.ClientActionID, b.Status, b.Cancelled, deadline, b.ExpiresAt, enc)
		} else {
			var changed sql.Result
			changed, err = tx.ExecContext(ctx, "UPDATE save_batch SET status=?,cancel_requested=?,state_enc=?,version=version+1 WHERE id=? AND user_id=? AND version=?", b.Status, b.Cancelled, enc, b.ID, b.User, b.Version)
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
		for _, x := range b.Items {
			_, err = tx.ExecContext(ctx, `INSERT INTO save_batch_item(id,user_id,batch_id,asset_id,order_no,prepare_status,delivery_mode,direct_fallback_used,copy_link_available,media_object_id,attempt,error_code) VALUES (?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE prepare_status=VALUES(prepare_status),delivery_mode=VALUES(delivery_mode),direct_fallback_used=VALUES(direct_fallback_used),copy_link_available=VALUES(copy_link_available),media_object_id=VALUES(media_object_id),attempt=VALUES(attempt),error_code=VALUES(error_code),version=version+1`, x.ID, b.User, b.ID, x.Asset.ID, x.Asset.Order, x.Status, x.Mode, x.Fallback, x.CopyAvailable, x.ObjectID, x.Attempt, x.Failure)
			if err != nil {
				return err
			}
			if err = s.saveDirect(ctx, tx, b.User, x); err != nil {
				return err
			}
			if err = s.commitDeliverySignals(ctx, tx, b, x, windows); err != nil {
				return err
			}
		}
	}
	for composite, rec := range s.keys {
		parts := strings.SplitN(composite, ":", 3)
		if len(parts) != 3 || parts[0] != c.User || (parts[1] != "prepare" && parts[1] != "batch-retry" && parts[1] != "batch-fallback") {
			continue
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO idempotency_record(id,user_id,operation,key_value,payload_hash,object_type,object_id,expires_at) VALUES (?,?,?,?,?,'save_batch',?,?) ON DUPLICATE KEY UPDATE id=id", auth.ID(), c.User, parts[1], parts[2], rec.Digest, rec.ID, rec.Expires)
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	deliveryLog(windows, s.hostWindows, s.Settings)
	s.hostWindows = windows
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
	seq++
	_, err = tx.ExecContext(ctx, "UPDATE save_batch_item SET lease_owner=?,lease_expires_at=?,claim_seq=?,prepare_status='PREPARING',version=version+1 WHERE id=?", owner, minTime(deadline, time.Now().Add(lease)), seq, iid)
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
	go s.renewMediaLease(call, cancel, iid, user, owner, seq, deadline, lease)
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

func (s *Mock) renewMediaLease(ctx context.Context, cancel context.CancelFunc, id, user, owner string, seq uint64, deadline time.Time, lease time.Duration) {
	ticker := time.NewTicker(lease / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := s.Sessions.DB.ExecContext(ctx, `UPDATE save_batch_item SET lease_expires_at=?,version=version+1
				WHERE id=? AND user_id=? AND claim_seq=? AND lease_owner=? AND prepare_status='PREPARING' AND lease_expires_at>UTC_TIMESTAMP(3)`,
				minTime(deadline, time.Now().Add(lease)), id, user, seq, owner)
			if err == nil {
				var changed int64
				changed, err = result.RowsAffected()
				if err == nil && changed == 1 {
					continue
				}
			}
			cancel()
			return
		}
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
