package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/parse"
)

// Persistent parsing uses the existing normalized tables. Source URLs and
// candidate lists are encrypted and bound to their row ID and purpose.
type persistentParse struct {
	Job                *job
	Source, SourceHash string
	Refresh            bool
	Claim              uint64
	Owner              string
}

func (s *Mock) adapterVersion() string {
	if s.AdapterTag != "" {
		return s.AdapterTag
	}
	return parse.MockVersion
}

func (s *Mock) ConfigureTaskKeys(raw string) error {
	s.TaskKeys = make(map[string][]byte)
	latest := 0
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return errors.New("invalid data encryption key configuration")
		}
		n, e := strconv.Atoi(strings.TrimPrefix(parts[0], "v"))
		key, e2 := base64.StdEncoding.DecodeString(parts[1])
		if e != nil || e2 != nil || n < 1 || parts[0] != "v"+strconv.Itoa(n) || len(key) != 32 || s.TaskKeys[parts[0]] != nil {
			return errors.New("invalid data encryption key configuration")
		}
		s.TaskKeys[parts[0]] = key
		if n > latest {
			latest = n
			s.TaskKeyVersion = parts[0]
		}
	}
	return nil
}
func (s *Mock) taskCipher(version string) (cipher.AEAD, error) {
	key := s.TaskKeys[version]
	if len(key) != 32 {
		return nil, errors.New("data encryption key unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}
func (s *Mock) sealTask(id, purpose string, value any) (string, error) {
	a, err := s.taskCipher(s.TaskKeyVersion)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return s.TaskKeyVersion + ":" + base64.StdEncoding.EncodeToString(a.Seal(nil, nil, b, []byte(id+":"+purpose))), nil
}
func (s *Mock) openTask(id, purpose, raw string, out any) error {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return errors.New("invalid encrypted task value")
	}
	a, err := s.taskCipher(parts[0])
	if err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	b, err = a.Open(nil, nil, b, []byte(id+":"+purpose))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Mock) createPersistentParse(w http.ResponseWriter, r *http.Request, c credential, req jobRequest, selected, platform string) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 100 {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	raw, _ := json.Marshal(req)
	digest := auth.Digest(s.Sessions.Key, string(raw))
	ctx := r.Context()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	// The per-user lock serializes idempotency and active-count checks with deletion.
	if err = s.lockUser(ctx, tx, c); err != nil {
		privateWriteError(w, err)
		return
	}
	var id, prior string
	err = tx.QueryRowContext(ctx, "SELECT object_id,payload_hash FROM idempotency_record WHERE user_id=? AND operation='parse' AND key_value=?", c.User, key).Scan(&id, &prior)
	if err == nil {
		if prior != digest {
			fail(w, 409, "IDEMPOTENCY_CONFLICT")
			return
		}
		var j job
		err = tx.QueryRowContext(ctx, "SELECT id,status,deadline_at,expires_at FROM parse_job WHERE id=? AND user_id=?", id, c.User).Scan(&j.ID, &j.Status, &j.Deadline, &j.Expires)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !time.Now().Before(j.Expires) {
			fail(w, 410, "JOB_EXPIRED")
			return
		}
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		s.jobAccepted(w, &j)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	var active int64
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM parse_job WHERE user_id=? AND status IN ('QUEUED','RUNNING') AND deadline_at>UTC_TIMESTAMP(3)", c.User).Scan(&active)
	if err != nil || active >= s.Settings.Int("user_active_parse_jobs") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	s.mu.Lock()
	allowed := s.minute("parse:"+c.User, s.Settings.Int("user_parse_per_minute"))
	s.mu.Unlock()
	if !allowed {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	now := time.Now().UTC()
	j := job{ID: auth.ID(), User: c.User, Platform: platform, Status: "QUEUED", FlowID: req.FlowID, Created: now, Deadline: now.Add(s.Settings.Seconds("parse_job_timeout_seconds")), Expires: now.Add(s.Settings.Seconds("result_ttl_seconds"))}
	enc, err := s.sealTask(j.ID, "source", selected)
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO parse_job(id,user_id,flow_id,platform,status,source_hash,source_url_enc,source_expires_at,adapter_version,deadline_at,expires_at,force_refresh) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", j.ID, j.User, j.FlowID, j.Platform, j.Status, auth.Digest(s.Sessions.Key, selected), enc, j.Expires, s.adapterVersion(), j.Deadline, j.Expires, req.ForceRefresh)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO idempotency_record(id,user_id,operation,key_value,payload_hash,object_type,object_id,expires_at) VALUES (?,?,'parse',?,?,'parse_job',?,?)", auth.ID(), c.User, key, digest, j.ID, now.Add(s.Settings.Seconds("idempotency_ttl_seconds")))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	s.jobAccepted(w, &j)
}

func (s *Mock) claimParse(ctx context.Context, owner string) (*persistentParse, error) {
	// Expiry is an independent commit, even when no work can be claimed.
	_, err := s.Sessions.DB.ExecContext(ctx, "UPDATE parse_job SET status=IF(cancel_requested,'CANCELLED','FAILED'),error_code=IF(cancel_requested,NULL,'PARSE_TIMEOUT'),finished_at=UTC_TIMESTAMP(3),version=version+1 WHERE status IN ('QUEUED','RUNNING') AND (deadline_at<=UTC_TIMESTAMP(3) OR cancel_requested)")
	if err != nil {
		return nil, err
	}
	// A queue scan must not retain repeatable-read gap locks across publication.
	tx, err := s.Sessions.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p := &persistentParse{Job: &job{}, Owner: owner}
	var enc string
	// Lock only jobs. A joined/subquery user check can be flattened by MySQL,
	// locking users after jobs and deadlocking publication's user-first order.
	err = tx.QueryRowContext(ctx, `SELECT j.id,j.user_id,j.platform,j.flow_id,j.created_at,j.deadline_at,j.expires_at,j.source_url_enc,j.source_hash,j.force_refresh,j.claim_seq FROM parse_job j WHERE j.adapter_version=? AND j.cancel_requested=FALSE AND j.deadline_at>UTC_TIMESTAMP(3) AND (j.status='QUEUED' OR (j.status='RUNNING' AND j.lease_expires_at<=UTC_TIMESTAMP(3))) ORDER BY j.created_at,j.id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.adapterVersion()).Scan(&p.Job.ID, &p.Job.User, &p.Job.Platform, &p.Job.FlowID, &p.Job.Created, &p.Job.Deadline, &p.Job.Expires, &enc, &p.SourceHash, &p.Refresh, &p.Claim)
	if errors.Is(err, sql.ErrNoRows) {
		if e := tx.Commit(); e != nil {
			return nil, e
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	var userStatus string
	if e := tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=?", p.Job.User).Scan(&userStatus); e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if userStatus != "active" {
		if _, e := tx.ExecContext(ctx, "UPDATE parse_job SET status='CANCELLED',cancel_requested=TRUE,version=version+1 WHERE id=?", p.Job.ID); e != nil {
			return nil, e
		}
		if e := tx.Commit(); e != nil {
			return nil, e
		}
		return nil, sql.ErrNoRows
	}
	if err = s.openTask(p.Job.ID, "source", enc, &p.Source); err != nil {
		_, e := tx.ExecContext(ctx, "UPDATE parse_job SET status='FAILED',error_code='UPSTREAM_INVALID_RESPONSE',version=version+1 WHERE id=?", p.Job.ID)
		if e != nil {
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		return nil, err
	}
	p.Claim++
	_, err = tx.ExecContext(ctx, "UPDATE parse_job SET status='RUNNING',lease_owner=?,lease_expires_at=?,claim_seq=?,version=version+1,started_at=UTC_TIMESTAMP(3),attempt=attempt+1 WHERE id=?", owner, minTime(p.Job.Deadline, time.Now().Add(s.Settings.Seconds("parser_call_timeout_seconds"))), p.Claim, p.Job.ID)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

func (s *Mock) finishParse(ctx context.Context, p *persistentParse, parsed *parse.Result, parseErr error) error {
	ctx, cancel := context.WithDeadline(ctx, minTime(p.Job.Deadline, time.Now().Add(s.Settings.Seconds("parser_call_timeout_seconds"))))
	defer cancel()
	for {
		err := s.finishParseOnce(ctx, p, parsed, parseErr)
		var conflict *mysql.MySQLError
		if !errors.As(err, &conflict) || conflict.Number != 1213 {
			return err
		}
		// Retry only the rolled-back database publication, never the parser.
		// The job/lease fence is checked again on every attempt.
		timer := time.NewTimer(time.Duration(s.Settings.Int("poll_interval_ms")) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Mock) finishParseOnce(ctx context.Context, p *persistentParse, parsed *parse.Result, parseErr error) error {
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the user before the job, matching creation and privacy deletion.
	var status string
	if err = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", p.Job.User).Scan(&status); err != nil {
		return err
	}
	var seq uint64
	var owner, jobStatus string
	var canceled bool
	var deadline, lease time.Time
	err = tx.QueryRowContext(ctx, "SELECT status,claim_seq,lease_owner,cancel_requested,deadline_at,lease_expires_at FROM parse_job WHERE id=? AND user_id=? FOR UPDATE", p.Job.ID, p.Job.User).Scan(&jobStatus, &seq, &owner, &canceled, &deadline, &lease)
	if err != nil {
		return err
	}
	if status != "active" || jobStatus != "RUNNING" || seq != p.Claim || owner != p.Owner || canceled || !time.Now().Before(lease) {
		return nil
	}
	code := ""
	if parseErr != nil {
		code = parseError(parseErr)
	}
	if !time.Now().Before(deadline) {
		code = "PARSE_TIMEOUT"
	}
	if code != "" {
		_, err = tx.ExecContext(ctx, "UPDATE parse_job SET status='FAILED',error_code=?,finished_at=UTC_TIMESTAMP(3),version=version+1 WHERE id=?", code, p.Job.ID)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	resID := auth.ID()
	warnings, _ := json.Marshal(parsed.Warnings)
	history, err := s.sealTask(resID, "history", p.Source)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO parse_result(id,user_id,job_id,platform,content_id,media_type,title,description,author_name,warnings,expires_at,source_hash,adapter_version,history_url_enc) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", resID, p.Job.User, p.Job.ID, parsed.Platform, parsed.ContentID, parsed.MediaType, parsed.Title, parsed.Description, parsed.AuthorName, string(warnings), p.Job.Expires, p.SourceHash, s.adapterVersion(), history)
	if err != nil {
		return err
	}
	assets := append([]parse.Asset{}, parsed.Assets...)
	if parsed.Cover != nil {
		assets = append(assets, *parsed.Cover)
	}
	for _, a := range assets {
		id := auth.ID()
		sources := append([]string{a.Source}, a.Alternates...)
		enc, e := s.sealTask(id, "candidates", sources)
		if e != nil {
			return e
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO media_asset(id,user_id,result_id,type,role,order_no,source_candidates_enc,source_expiry,availability) VALUES (?,?,?,?,?,?,?,?,'candidate')", id, p.Job.User, resID, a.Kind, a.Role, a.Order, enc, p.Job.Expires)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE parse_job SET status='SUCCEEDED',result_id=?,finished_at=UTC_TIMESTAMP(3),version=version+1 WHERE id=?", resID, p.Job.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RunPersistentParse starts a fixed number of workers; jobs remain queued in
// MySQL until a worker claims them. Restart recovery uses expired leases.
func (s *Mock) RunPersistentParse(ctx context.Context) {
	for i := int64(0); i < s.Settings.Int("parser_workers"); i++ {
		go s.parseWorker(ctx, auth.ID())
	}
}
func (s *Mock) parseWorker(ctx context.Context, owner string) {
	interval := time.Duration(s.Settings.Int("poll_interval_ms")) * time.Millisecond
	for ctx.Err() == nil {
		p, err := s.claimParse(ctx, owner)
		if err == nil {
			call, cancel := context.WithDeadline(ctx, p.Job.Deadline)
			stopped := make(chan struct{})
			go func() {
				ticker := time.NewTicker(s.Settings.Seconds("parser_call_timeout_seconds") / 2)
				defer ticker.Stop()
				for {
					select {
					case <-stopped:
						return
					case <-call.Done():
						return
					case <-ticker.C:
						r, e := s.Sessions.DB.ExecContext(call, "UPDATE parse_job SET lease_expires_at=LEAST(deadline_at,?),version=version+1 WHERE id=? AND status='RUNNING' AND claim_seq=? AND lease_owner=? AND cancel_requested=FALSE AND lease_expires_at>UTC_TIMESTAMP(3)", time.Now().Add(s.Settings.Seconds("parser_call_timeout_seconds")), p.Job.ID, p.Claim, owner)
						if e != nil {
							cancel()
							return
						}
						n, _ := r.RowsAffected()
						if n != 1 {
							cancel()
							return
						}
					}
				}
			}()
			parsed, e := s.parseCache.resolve(call, p.SourceHash, p.Refresh, func() (*parse.Result, error) { return s.callParser(call, p.Source) })
			close(stopped)
			if ctx.Err() == nil {
				if err := s.finishParse(ctx, p, parsed, e); err != nil {
					slog.Warn("parse publication failed", "error", err)
				}
			}
			cancel()
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

func (s *Mock) persistentJobStatus(w http.ResponseWriter, r *http.Request, c credential) {
	ctx := r.Context()
	id := r.PathValue("jobId")
	if r.Method == http.MethodDelete {
		_, err := s.Sessions.DB.ExecContext(ctx, "UPDATE parse_job SET status='CANCELLED',cancel_requested=TRUE,version=version+1 WHERE id=? AND user_id=? AND status IN ('QUEUED','RUNNING')", id, c.User)
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
	}
	j := &job{cancel: func() {}}
	var resultID, failure sql.NullString
	err := s.Sessions.DB.QueryRowContext(ctx, "SELECT id,user_id,platform,flow_id,status,result_id,error_code,created_at,deadline_at,expires_at FROM parse_job WHERE id=? AND user_id=?", id, c.User).Scan(&j.ID, &j.User, &j.Platform, &j.FlowID, &j.Status, &resultID, &failure, &j.Created, &j.Deadline, &j.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	j.ResultID, j.Failure = resultID.String, failure.String
	if !time.Now().Before(j.Expires) {
		fail(w, 410, "JOB_EXPIRED")
		return
	}
	if j.active() && !time.Now().Before(j.Deadline) {
		_, err = s.Sessions.DB.ExecContext(ctx, "UPDATE parse_job SET status='FAILED',error_code='PARSE_TIMEOUT',version=version+1 WHERE id=? AND user_id=? AND status IN ('QUEUED','RUNNING')", id, c.User)
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		j.Status, j.Failure = "FAILED", "PARSE_TIMEOUT"
	}
	if j.ResultID != "" {
		res, e := s.loadPersistentResult(ctx, c.User, j.ResultID)
		if e != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		j.History = res.history
	}
	// Render via the existing API contract without sharing mutable state.
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	view := Mock{jobs: map[string]*job{j.ID: j}}
	view.jobStatus(w, r, c)
}

type persistentResult struct {
	*result
	history *historySource
}

func (s *Mock) loadPersistentResult(ctx context.Context, user, id string) (*persistentResult, error) {
	res := &result{Value: &parse.Result{}}
	v := res.Value
	var warnings, history string
	err := s.Sessions.DB.QueryRowContext(ctx, "SELECT id,user_id,platform,content_id,media_type,title,description,author_name,warnings,created_at,expires_at,COALESCE(history_url_enc,'') FROM parse_result WHERE id=? AND user_id=?", id, user).Scan(&res.ID, &res.User, &v.Platform, &v.ContentID, &v.MediaType, &v.Title, &v.Description, &v.AuthorName, &warnings, &res.CreatedAt, &res.ExpiresAt, &history)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(warnings), &v.Warnings); err != nil {
		return nil, err
	}
	out := &persistentResult{result: res}
	if !time.Now().Before(res.ExpiresAt) {
		return out, nil
	}
	if history != "" {
		var source string
		if err = s.openTask(id, "history", history, &source); err != nil {
			return nil, err
		}
		u, e := url.Parse(source)
		if e != nil {
			return nil, e
		}
		u.Host = strings.ToLower(u.Host)
		u.Fragment, u.RawFragment = "", ""
		out.history = &historySource{source, auth.Digest(s.Sessions.Key, "history:"+user+":"+u.String())}
	}
	rows, err := s.Sessions.DB.QueryContext(ctx, "SELECT id,type,role,order_no,version,source_candidates_enc FROM media_asset WHERE result_id=? AND user_id=? ORDER BY role,order_no", id, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a asset
		var enc string
		if err = rows.Scan(&a.ID, &a.Kind, &a.Role, &a.Order, &a.Version, &enc); err != nil {
			return nil, err
		}
		var sources []string
		if err = s.openTask(a.ID, "candidates", enc, &sources); err != nil {
			return nil, err
		}
		if len(sources) == 0 {
			return nil, errors.New("empty media candidates")
		}
		p := parse.Asset{Kind: a.Kind, Role: a.Role, Order: a.Order, Source: sources[0], Alternates: sources[1:]}
		if a.Role == "cover" {
			res.Cover = &a
			v.Cover = &p
		} else {
			res.Assets = append(res.Assets, a)
			v.Assets = append(v.Assets, p)
		}
	}
	return out, rows.Err()
}
