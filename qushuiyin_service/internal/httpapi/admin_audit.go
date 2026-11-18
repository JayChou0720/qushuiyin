package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"qushuiyin/service/internal/auth"
)

func (s *Mock) adminAuditList(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	page, okPage := s.readAdminPage(w, r)
	if !okPage {
		return
	}
	query := "SELECT a.id,a.operator_id,a.action,a.target_type,a.target_id,a.before_redacted,a.after_redacted,a.reason,a.request_id,a.created_at FROM admin_audit a WHERE a.created_at>?"
	args := []any{time.Now().UTC().AddDate(0, 0, -int(s.Settings.Int("admin_audit_retention_days")))}
	for _, filter := range []struct{ param, column string }{{"targetType", "a.target_type"}, {"targetId", "a.target_id"}, {"action", "a.action"}} {
		value := r.URL.Query().Get(filter.param)
		if value == "" {
			continue
		}
		if !opaqueID(value) {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		query += " AND " + filter.column + "=?"
		args = append(args, value)
	}
	query, args = page.query(query, "a", args)
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		var id, operator, action, targetType, targetID, reason, requestID string
		var before, after []byte
		var created time.Time
		if err = rows.Scan(&id, &operator, &action, &targetType, &targetID, &before, &after, &reason, &requestID, &created); err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, dto{"id": id, "operatorId": operator, "action": action, "targetType": targetType, "targetId": targetID, "before": json.RawMessage(before), "after": json.RawMessage(after), "reason": reason, "requestId": requestID, "createdAt": created})
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, page.response(items))
}
