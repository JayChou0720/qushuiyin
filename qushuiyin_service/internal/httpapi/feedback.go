package httpapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/parse"
)

type feedbackRequest struct {
	Category, Content, Contact, TraceID, SourceURL string
	IncludeSource                                  bool
}

func (s *Mock) lockUser(ctx context.Context, tx *sql.Tx, c credential) error {
	var status string
	err := tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", c.User).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status != "active") {
		return auth.ErrSession
	}
	if err != nil {
		return err
	}
	var active int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM app_session WHERE user_id=? AND token_digest=? AND revoked_at IS NULL AND expires_at>UTC_TIMESTAMP(3)", c.User, c.Digest).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.ErrSession
	}
	return err
}

func privateWriteError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrSession) {
		fail(w, 401, "SESSION_EXPIRED")
		return
	}
	fail(w, 503, "SERVICE_BUSY")
}

func (s *Mock) encryptFeedback(id, field, value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(auth.Digest(s.Sessions.Key, "local feedback encryption v1"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(value), []byte(id+":"+field))), nil
}

var shanghaiDay = time.FixedZone("Asia/Shanghai", 8*60*60)

func dayStart(now time.Time) time.Time {
	local := now.In(shanghaiDay)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghaiDay).UTC()
}

func (s *Mock) submitFeedback(w http.ResponseWriter, r *http.Request, c credential) {
	var req feedbackRequest
	if !s.read(w, r, &req) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	req.Content = strings.TrimSpace(req.Content)
	req.Contact = strings.TrimSpace(req.Contact)
	length := int64(utf8.RuneCountInString(req.Content))
	if !feedbackCategory(req.Category) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if key == "" || len(key) > 100 || length < s.Settings.Int("feedback_min_chars") || length > s.Settings.Int("feedback_max_chars") || int64(utf8.RuneCountInString(req.Contact)) > s.Settings.Int("feedback_contact_max_chars") || (req.TraceID != "" && !opaqueID(req.TraceID)) || (!req.IncludeSource && req.SourceURL != "") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if req.SourceURL != "" {
		u, err := url.Parse(req.SourceURL)
		if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || parse.Platform(u) == "" || int64(len(req.SourceURL)) > s.Settings.Int("max_share_text_chars") {
			fail(w, 400, "LINK_NOT_ALLOWED")
			return
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	ctx := r.Context()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	defer tx.Rollback()
	if err = s.lockUser(ctx, tx, c); err != nil {
		privateWriteError(w, err)
		return
	}
	payload, _ := json.Marshal(req)
	hash := auth.Digest(s.Sessions.Key, string(payload))
	var prevID, prevHash string
	err = tx.QueryRowContext(ctx, "SELECT object_id,payload_hash FROM idempotency_record WHERE user_id=? AND operation='feedback' AND key_value=? AND expires_at>?", c.User, key, now).Scan(&prevID, &prevHash)
	if err == nil {
		if hash != prevHash {
			fail(w, 409, "IDEMPOTENCY_CONFLICT")
			return
		}
		var created time.Time
		err = tx.QueryRowContext(ctx, "SELECT created_at FROM feedback WHERE user_id=? AND id=? AND expires_at>?", c.User, prevID, now).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		if err != nil {
			privateWriteError(w, err)
			return
		}
		ok(w, 201, dto{"feedbackId": prevID, "status": "pending", "createdAt": created})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		privateWriteError(w, err)
		return
	}
	if req.TraceID != "" {
		s.mu.Lock()
		j := s.jobs[req.TraceID]
		owned := j != nil && j.User == c.User && j.Status == "FAILED" && now.Before(j.Expires)
		s.mu.Unlock()
		if !owned {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
	}
	start := dayStart(now)
	var n int64
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM feedback WHERE user_id=? AND created_at>=?", c.User, start).Scan(&n)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	if n >= s.Settings.Int("user_feedback_per_day") {
		retry := int64(start.AddDate(0, 0, 1).Sub(now)/time.Second) + 1
		w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
		send(w, 429, "DAILY_LIMIT_REACHED", nil, dto{"retryAfterSeconds": retry})
		return
	}
	id := auth.ID()
	contact, err := s.encryptFeedback(id, "contact", req.Contact)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	source, err := s.encryptFeedback(id, "source", req.SourceURL)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	expiry := now.AddDate(0, 0, int(s.Settings.Int("feedback_retention_days")))
	_, err = tx.ExecContext(ctx, "INSERT INTO feedback(id,user_id,category,content,contact_enc,trace_id,source_url_enc,source_consent,status,created_at,updated_at,expires_at) VALUES (?,?,?,?,?,NULLIF(?,''),?,?,'pending',?,?,?)", id, c.User, req.Category, req.Content, contact, req.TraceID, source, req.IncludeSource, now, now, expiry)
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM idempotency_record WHERE user_id=? AND operation='feedback' AND key_value=? AND expires_at<=?", c.User, key, now)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO idempotency_record(id,user_id,operation,key_value,payload_hash,object_type,object_id,expires_at) VALUES (?,?,'feedback',?,?,'feedback',?,?)", auth.ID(), c.User, key, hash, id, now.Add(s.Settings.Seconds("idempotency_ttl_seconds")))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		privateWriteError(w, err)
		return
	}
	ok(w, 201, dto{"feedbackId": id, "status": "pending", "createdAt": now})
}

func (s *Mock) feedbackList(w http.ResponseWriter, r *http.Request, c credential) {
	limit := s.Settings.Int("pagination_default")
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 || n > s.Settings.Int("pagination_max") {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		limit = n
	}
	ctx := r.Context()
	cursor := r.URL.Query().Get("cursor")
	query := "SELECT id,category,LEFT(content,80),status,created_at,updated_at FROM feedback WHERE user_id=? AND expires_at>UTC_TIMESTAMP(3)"
	args := []any{c.User}
	if cursor != "" {
		if !opaqueID(cursor) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		var created time.Time
		err := s.Sessions.DB.QueryRowContext(ctx, "SELECT created_at FROM feedback WHERE user_id=? AND id=? AND expires_at>UTC_TIMESTAMP(3)", c.User, cursor).Scan(&created)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		if err != nil {
			privateWriteError(w, err)
			return
		}
		query += " AND (created_at<? OR (created_at=? AND id<?))"
		args = append(args, created, created, cursor)
	}
	rows, err := s.Sessions.DB.QueryContext(ctx, query+" ORDER BY created_at DESC,id DESC LIMIT ?", append(args, limit+1)...)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	defer rows.Close()
	items := make([]dto, 0)
	for rows.Next() {
		var id, category, summary, status string
		var created, updated time.Time
		if err = rows.Scan(&id, &category, &summary, &status, &created, &updated); err != nil {
			privateWriteError(w, err)
			return
		}
		items = append(items, dto{"id": id, "category": category, "summary": summary, "status": status, "createdAt": created, "updatedAt": updated})
	}
	if err = rows.Err(); err != nil {
		privateWriteError(w, err)
		return
	}
	var next any
	more := int64(len(items)) > limit
	if more {
		items = items[:limit]
		next = items[len(items)-1]["id"]
	}
	ok(w, 200, dto{"items": items, "nextCursor": next, "hasMore": more})
}

func (s *Mock) feedbackDetail(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("feedbackId")
	var category, content, status string
	var reply sql.NullString
	var contact bool
	var created, updated time.Time
	var replied sql.NullTime
	err := s.Sessions.DB.QueryRowContext(r.Context(), "SELECT category,content,status,contact_enc IS NOT NULL,reply,replied_at,created_at,updated_at FROM feedback WHERE user_id=? AND id=? AND expires_at>UTC_TIMESTAMP(3)", c.User, id).Scan(&category, &content, &status, &contact, &reply, &replied, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if err != nil {
		privateWriteError(w, err)
		return
	}
	data := dto{"id": id, "category": category, "content": content, "status": status, "contact": nil, "reply": nil, "repliedAt": nil, "createdAt": created, "updatedAt": updated}
	if contact {
		data["contact"] = "***"
	}
	if reply.Valid {
		data["reply"] = reply.String
	}
	if replied.Valid {
		data["repliedAt"] = replied.Time
	}
	ok(w, 200, data)
}
