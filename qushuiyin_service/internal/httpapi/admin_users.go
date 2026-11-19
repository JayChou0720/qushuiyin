package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/go-sql-driver/mysql"
	"qushuiyin/service/internal/auth"
)

const selectAdminAccount = "SELECT u.id,u.username,u.role,u.status,u.version,u.created_at,u.last_login_at,u.locked_until FROM admin_user u WHERE 1=1"

func readAdminAccount(row rowScanner) (dto, error) {
	var id, username, role, status string
	var version int64
	var created time.Time
	var login, locked *time.Time
	err := row.Scan(&id, &username, &role, &status, &version, &created, &login, &locked)
	return dto{"id": id, "username": username, "role": role, "status": status, "version": version, "createdAt": created, "lastLoginAt": login, "lockedUntil": locked}, err
}
func adminAccountRole(role string) bool {
	return oneOf(role, "super_admin", "tech_admin", "operator", "support", "auditor", "viewer")
}

func (s *Mock) adminUsers(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	page, okPage := s.readAdminPage(w, r)
	if !okPage {
		return
	}
	query, args := page.query(selectAdminAccount, "u", nil)
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		item, err := readAdminAccount(rows)
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, page.response(items))
}

func (s *Mock) hashAdminPassword(w http.ResponseWriter, raw string) (string, bool) {
	password := []byte(raw)
	defer clear(password)
	if !auth.ValidAdminPassword(password) {
		fail(w, 400, "BAD_REQUEST")
		return "", false
	}
	select {
	case s.adminHashSlots <- struct{}{}:
		defer func() { <-s.adminHashSlots }()
		return auth.HashAdminPassword(password), true
	default:
		fail(w, 503, "SERVICE_BUSY")
		return "", false
	}
}

// Lock the active super-admin set before any account mutation, and recheck the actor.
func lockSuperAdmins(ctx context.Context, tx *sql.Tx, actor string) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM admin_user WHERE role='super_admin' AND status='active' ORDER BY id FOR UPDATE")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n, allowed := 0, false
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return 0, err
		}
		n++
		allowed = allowed || actor == id
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if !allowed {
		return 0, auth.ErrSession
	}
	return n, nil
}

func adminAccountError(w http.ResponseWriter, err error) {
	var mysqlErr *mysql.MySQLError
	switch {
	case errors.Is(err, auth.ErrSession):
		fail(w, 403, "ACCESS_DENIED")
	case errors.As(err, &mysqlErr) && mysqlErr.Number == 1062:
		fail(w, 409, "VERSION_CONFLICT")
	default:
		adminFeedbackReadError(w, err)
	}
}

func (s *Mock) adminUserCreate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct{ Username, Role, Password, Reason string }
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	if !valid || !auth.ValidAdminUsername(req.Username) || !adminAccountRole(req.Role) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	hash, valid := s.hashAdminPassword(w, req.Password)
	if !valid {
		return
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		adminAccountError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = lockSuperAdmins(ctx, tx, a.ID); err != nil {
		adminAccountError(w, err)
		return
	}
	id := auth.ID()
	_, err = tx.ExecContext(ctx, "INSERT INTO admin_user(id,username,password_hash,role,status) VALUES (?,?,?,?,'active')", id, req.Username, hash, req.Role)
	if err == nil {
		err = auditChange(ctx, tx, a.ID, "admin_create", "admin_user", id, reason, requestID(w), nil, dto{"username": req.Username, "role": req.Role, "status": "active"})
	}
	var item dto
	if err == nil {
		item, err = readAdminAccount(tx.QueryRowContext(ctx, selectAdminAccount+" AND u.id=?", id))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		adminAccountError(w, err)
		return
	}
	ok(w, 201, item)
}

func (s *Mock) adminUserUpdate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct {
		ExpectedVersion        int64
		Role, Status, Password *string
		Reason                 string
	}
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	id := r.PathValue("id")
	if !valid || !opaqueID(id) || req.ExpectedVersion < 1 || (req.Role == nil && req.Status == nil && req.Password == nil) ||
		(req.Role != nil && !adminAccountRole(*req.Role)) || (req.Status != nil && !oneOf(*req.Status, "active", "disabled")) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	var hash string
	if req.Password != nil {
		hash, valid = s.hashAdminPassword(w, *req.Password)
		if !valid {
			return
		}
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		adminAccountError(w, err)
		return
	}
	defer tx.Rollback()
	active, err := lockSuperAdmins(ctx, tx, a.ID)
	if err != nil {
		adminAccountError(w, err)
		return
	}
	before, err := readAdminAccount(tx.QueryRowContext(ctx, selectAdminAccount+" AND u.id=? FOR UPDATE", id))
	if err != nil {
		adminAccountError(w, err)
		return
	}
	if before["version"] != req.ExpectedVersion {
		fail(w, 409, "VERSION_CONFLICT")
		return
	}
	role, status := before["role"].(string), before["status"].(string)
	if req.Role != nil {
		role = *req.Role
	}
	if req.Status != nil {
		status = *req.Status
	}
	if before["role"] == "super_admin" && before["status"] == "active" && active == 1 && (role != "super_admin" || status != "active") {
		fail(w, 409, "LAST_SUPER_ADMIN")
		return
	}
	if role == before["role"] && status == before["status"] && hash == "" {
		ok(w, 200, before)
		return
	}
	_, err = tx.ExecContext(ctx, `UPDATE admin_user SET role=?,status=?,version=version+1,password_hash=IF(?='',password_hash,?),locked_until=IF(?='',locked_until,NULL),
		failed_login_count=IF(?='',failed_login_count,0) WHERE id=?`, role, status, hash, hash, hash, hash, id)
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE admin_session SET revoked_at=UTC_TIMESTAMP(3) WHERE admin_user_id=? AND revoked_at IS NULL", id)
	}
	if err == nil {
		err = auditChange(ctx, tx, a.ID, "admin_update", "admin_user", id, reason, requestID(w),
			dto{"role": before["role"], "status": before["status"], "version": before["version"]}, dto{"role": role, "status": status, "version": req.ExpectedVersion + 1, "passwordReset": req.Password != nil})
	}
	var item dto
	if err == nil {
		item, err = readAdminAccount(tx.QueryRowContext(ctx, selectAdminAccount+" AND u.id=?", id))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		adminAccountError(w, err)
		return
	}
	ok(w, 200, item)
}
