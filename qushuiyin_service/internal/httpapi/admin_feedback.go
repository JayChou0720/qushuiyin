package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
)

const activeFeedback = " FROM feedback f JOIN app_user u ON u.id=f.user_id WHERE u.status='active' AND f.expires_at>UTC_TIMESTAMP(3)"

func feedbackStatus(value string) bool {
	return oneOf(value, "pending", "processing", "replied", "closed")
}
func feedbackCategory(value string) bool {
	return oneOf(value, "parse_failed", "download_failed", "album_failed", "gallery_issue", "suggestion", "other")
}

type adminPage struct {
	limit int64
	at    time.Time
	id    string
}

func (s *Mock) readAdminPage(w http.ResponseWriter, r *http.Request) (adminPage, bool) {
	p := adminPage{limit: s.settings().Int("pagination_default")}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 1 || limit > s.settings().Int("pagination_max") {
			fail(w, 400, "BAD_REQUEST")
			return p, false
		}
		p.limit = limit
	}
	if value := r.URL.Query().Get("cursor"); value != "" {
		part, id, ok := strings.Cut(value, ":")
		milli, err := strconv.ParseInt(part, 10, 64)
		at := time.UnixMilli(milli).UTC()
		if !ok || err != nil || at.Year() < 1000 || at.Year() > 9999 || !opaqueID(id) {
			fail(w, 400, "BAD_REQUEST")
			return p, false
		}
		p.at, p.id = at, id
	}
	return p, true
}

func (p adminPage) query(query, alias string, args []any) (string, []any) {
	created, id := alias+".created_at", alias+".id"
	if p.id != "" {
		query += " AND (" + created + "<? OR (" + created + "=? AND " + id + "<?))"
		args = append(args, p.at, p.at, p.id)
	}
	return query + " ORDER BY " + created + " DESC," + id + " DESC LIMIT ?", append(args, p.limit+1)
}

func (p adminPage) response(items []dto) dto {
	var next any
	more := int64(len(items)) > p.limit
	if more {
		items = items[:p.limit]
		last := items[len(items)-1]
		next = strconv.FormatInt(last["createdAt"].(time.Time).UnixMilli(), 10) + ":" + last["id"].(string)
	}
	return dto{"items": items, "nextCursor": next, "hasMore": more}
}

func (s *Mock) adminFeedbackList(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	page, okPage := s.readAdminPage(w, r)
	if !okPage {
		return
	}
	query := "SELECT f.id,f.category,LEFT(f.content,80),f.status,f.version,f.created_at,f.updated_at" + activeFeedback
	var args []any
	if status := r.URL.Query().Get("status"); status != "" {
		if !feedbackStatus(status) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		query += " AND f.status=?"
		args = append(args, status)
	}
	if category := r.URL.Query().Get("category"); category != "" {
		if !feedbackCategory(category) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		query += " AND f.category=?"
		args = append(args, category)
	}
	query, args = page.query(query, "f", args)
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		var id, category, summary, status string
		var version int64
		var created, updated time.Time
		if err = rows.Scan(&id, &category, &summary, &status, &version, &created, &updated); err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, dto{"id": id, "category": category, "summary": summary, "status": status, "version": version, "createdAt": created, "updatedAt": updated})
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, page.response(items))
}

type adminFeedback struct {
	id, category, content, status string
	version                       int64
	reply                         sql.NullString
	replied                       sql.NullTime
	created, updated, expiry      time.Time
}

const feedbackFields = "SELECT f.id,f.category,f.content,f.status,f.version,f.reply,f.replied_at,f.created_at,f.updated_at,f.expires_at"

func (f *adminFeedback) scan(row *sql.Row) error {
	return row.Scan(&f.id, &f.category, &f.content, &f.status, &f.version, &f.reply, &f.replied, &f.created, &f.updated, &f.expiry)
}
func (f adminFeedback) public() dto {
	result := dto{"id": f.id, "category": f.category, "content": f.content, "status": f.status, "version": f.version, "reply": nil, "repliedAt": nil, "createdAt": f.created, "updatedAt": f.updated}
	if f.reply.Valid {
		result["reply"] = f.reply.String
	}
	if f.replied.Valid {
		result["repliedAt"] = f.replied.Time
	}
	return result
}
func adminFeedbackReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	fail(w, 503, "SERVICE_BUSY")
}
func (s *Mock) adminFeedbackDetail(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	var f adminFeedback
	if err := f.scan(s.Admin.DB.QueryRowContext(r.Context(), feedbackFields+activeFeedback+" AND f.id=?", r.PathValue("id"))); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	ok(w, 200, f.public())
}

func (s *Mock) adminFeedbackPrivate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct{ Reason string }
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	if !valid {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	id := r.PathValue("id")
	// Use the same owner-then-feedback lock order as deletion and updates.
	var user, status string
	if err = tx.QueryRowContext(ctx, "SELECT user_id FROM feedback WHERE id=?", id).Scan(&user); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if err = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR SHARE", user).Scan(&status); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if status != "active" {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	var contact, source, trace sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT contact_enc,IF(source_consent,source_url_enc,NULL),trace_id FROM feedback WHERE id=? AND user_id=? AND expires_at>UTC_TIMESTAMP(3) FOR SHARE", id, user).Scan(&contact, &source, &trace)
	if err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	contactValue, err := s.decryptFeedback(id, "contact", contact)
	var sourceValue, traceValue any
	if err == nil {
		sourceValue, err = s.decryptFeedback(id, "source", source)
	}
	if trace.Valid {
		traceValue = trace.String
	}
	if err == nil {
		err = auditChange(ctx, tx, a.ID, "feedback_private_view", "feedback", id, reason, requestID(w), nil,
			dto{"hasContact": contact.Valid, "hasSource": source.Valid, "hasTrace": trace.Valid})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, dto{"contact": contactValue, "sourceUrl": sourceValue, "traceId": traceValue})
}

func (s *Mock) adminFeedbackUpdate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct {
		ExpectedVersion int64
		Status, Reply   *string
		Reason          string
	}
	if !s.read(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.ExpectedVersion < 1 || req.Reason == "" || utf8.RuneCountInString(req.Reason) > 500 || (req.Status == nil && req.Reply == nil) || (req.Status != nil && !feedbackStatus(*req.Status)) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if req.Reply != nil {
		reply := strings.TrimSpace(*req.Reply)
		if reply == "" || int64(utf8.RuneCountInString(reply)) > s.settings().Int("feedback_max_chars") {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		req.Reply = &reply
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	id := r.PathValue("id")
	var user, status string
	// Take locks in the same order as user data deletion: owner, then feedback.
	if err = tx.QueryRowContext(ctx, "SELECT user_id FROM feedback WHERE id=?", id).Scan(&user); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if err = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", user).Scan(&status); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if status != "active" {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	var before adminFeedback
	if err = before.scan(tx.QueryRowContext(ctx, feedbackFields+" FROM feedback f WHERE f.id=? AND f.expires_at>UTC_TIMESTAMP(3) FOR UPDATE", id)); err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if req.ExpectedVersion != before.version {
		fail(w, 409, "VERSION_CONFLICT")
		return
	}
	after := before
	if req.Status != nil {
		after.status = *req.Status
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	replyChanged := req.Reply != nil && (!before.reply.Valid || *req.Reply != before.reply.String)
	if replyChanged {
		after.reply = sql.NullString{String: *req.Reply, Valid: true}
		after.replied = sql.NullTime{Time: now, Valid: true}
	}
	if after.status == "replied" && !after.reply.Valid {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if before.status == after.status && !replyChanged {
		ok(w, 200, before.public())
		return
	}
	if before.status == "closed" {
		fail(w, 409, "FEEDBACK_CLOSED")
		return
	}
	if after.status == "closed" {
		after.expiry = minTime(before.expiry, now.AddDate(0, 0, int(s.settings().Int("feedback_closed_retention_days"))))
	}
	after.version++
	after.updated = now
	_, err = tx.ExecContext(ctx, "UPDATE feedback SET status=?,reply=?,replied_at=?,expires_at=?,version=?,updated_at=? WHERE id=?", after.status, after.reply, after.replied, after.expiry, after.version, after.updated, id)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	was, _ := json.Marshal(dto{"status": before.status, "version": before.version, "hasReply": before.reply.Valid})
	became, _ := json.Marshal(dto{"status": after.status, "version": after.version, "hasReply": after.reply.Valid, "replyChanged": replyChanged})
	_, err = tx.ExecContext(ctx, "INSERT INTO admin_audit(id,operator_id,action,target_type,target_id,before_redacted,after_redacted,reason,request_id,created_at) VALUES (?,?,'feedback_update','feedback',?,?,?,?,?,?)", auth.ID(), a.ID, id, string(was), string(became), req.Reason, requestID(w), now)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, after.public())
}
