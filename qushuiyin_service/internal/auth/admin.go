package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const adminHashPrefix = "$argon2id$v=19$m=65536,t=3,p=2$"
const adminHashMemory = 65536

var ErrAdminLogin = errors.New("invalid admin login")
var ErrBootstrapComplete = errors.New("the first administrator already exists")

type Admin struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}
type AdminStore struct {
	DB           *sql.DB
	Key          []byte
	TTL, Lockout time.Duration
	MaxFailures  int64
}

func ValidAdminUsername(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
func ValidAdminPassword(password []byte) bool { return len(password) >= 12 && len(password) <= 256 }
func adminPasswordKey(password, salt []byte) []byte {
	return argon2.IDKey(password, salt, 3, adminHashMemory, 2, 32)
}
func adminHash(password []byte) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	enc := base64.RawStdEncoding
	return adminHashPrefix + enc.EncodeToString(salt) + "$" + enc.EncodeToString(adminPasswordKey(password, salt))
}
func checkAdminHash(password []byte, hash string) bool {
	suffix, valid := strings.CutPrefix(hash, adminHashPrefix)
	a, b, split := strings.Cut(suffix, "$")
	enc := base64.RawStdEncoding
	salt, aErr := enc.DecodeString(a)
	want, bErr := enc.DecodeString(b)
	valid = valid && split && aErr == nil && bErr == nil && len(salt) == 16 && len(want) == 32
	if !valid {
		salt, want = make([]byte, 16), make([]byte, 32)
	}
	return subtle.ConstantTimeCompare(adminPasswordKey(password, salt), want) == 1 && valid
}
func (s AdminStore) Bootstrap(ctx context.Context, username string, password []byte) error {
	if !ValidAdminUsername(username) || !ValidAdminPassword(password) {
		return errors.New("invalid username or password length")
	}
	hash := adminHash(password)
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Serialize the empty-store check across independent CLI invocations.
	var acquired sql.NullInt64
	if err = conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT(DATABASE(), ':admin-bootstrap'),5)").Scan(&acquired); err != nil {
		return err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("cannot lock administrator bootstrap")
	}
	defer func() {
		q, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, e := conn.ExecContext(q, "DO RELEASE_LOCK(CONCAT(DATABASE(), ':admin-bootstrap'))"); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM admin_user)").Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrBootstrapComplete
	}
	id := ID()
	if _, err = tx.ExecContext(ctx, "INSERT INTO admin_user(id,username,password_hash,role,status) VALUES (?,?,?,'super_admin','active')", id, username, hash); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO admin_audit(id,operator_id,action,target_type,target_id,reason,request_id) VALUES (?, ?, 'bootstrap', 'admin_user', ?, 'CLI bootstrap', ?)", ID(), id, id, ID()); err != nil {
		return err
	}
	return tx.Commit()
}
func (s AdminStore) TokenDigest(token string) string { return Digest(s.Key, "admin session:"+token) }
func (s AdminStore) Login(ctx context.Context, username string, password []byte, ip string) (Admin, string, time.Time, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Admin{}, "", time.Time{}, err
	}
	defer tx.Rollback()
	var a Admin
	var hash, status string
	var count int64
	var locked, hadLock bool
	err = tx.QueryRowContext(ctx, "SELECT id,username,role,password_hash,status,failed_login_count,COALESCE(locked_until>UTC_TIMESTAMP(3),0),locked_until IS NOT NULL FROM admin_user WHERE username=? FOR UPDATE", username).Scan(&a.ID, &a.Username, &a.Role, &hash, &status, &count, &locked, &hadLock)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Admin{}, "", time.Time{}, err
	}
	matched := checkAdminHash(password, hash)
	if a.ID == "" || locked || status != "active" {
		return Admin{}, "", time.Time{}, ErrAdminLogin
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if !matched {
		if hadLock {
			count = 0
		}
		count++
		var until any
		if count >= s.MaxFailures {
			until = now.Add(s.Lockout)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE admin_user SET failed_login_count=?,locked_until=? WHERE id=?", count, until, a.ID); err != nil {
			return Admin{}, "", time.Time{}, err
		}
		if err = tx.Commit(); err != nil {
			return Admin{}, "", time.Time{}, err
		}
		return Admin{}, "", time.Time{}, ErrAdminLogin
	}
	token, expires := ID()+ID(), now.Add(s.TTL)
	if _, err = tx.ExecContext(ctx, "UPDATE admin_user SET failed_login_count=0,locked_until=NULL,last_login_at=? WHERE id=?", now, a.ID); err != nil {
		return Admin{}, "", time.Time{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO admin_session(id,admin_user_id,token_digest,expires_at,ip_hash) VALUES (?,?,?,?,?)", ID(), a.ID, s.TokenDigest(token), expires, Digest(s.Key, "admin ip:"+ip)); err != nil {
		return Admin{}, "", time.Time{}, err
	}
	if err = tx.Commit(); err != nil {
		return Admin{}, "", time.Time{}, err
	}
	return a, token, expires, nil
}
func (s AdminStore) Current(ctx context.Context, token string) (Admin, error) {
	var a Admin
	err := s.DB.QueryRowContext(ctx, "SELECT u.id,u.username,u.role FROM admin_session s JOIN admin_user u ON s.admin_user_id=u.id WHERE s.token_digest=? AND s.revoked_at IS NULL AND s.expires_at>UTC_TIMESTAMP(3) AND u.status='active'", s.TokenDigest(token)).Scan(&a.ID, &a.Username, &a.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrSession
	}
	return a, err
}
func (s AdminStore) Revoke(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE admin_session SET revoked_at=UTC_TIMESTAMP(3) WHERE token_digest=? AND revoked_at IS NULL", s.TokenDigest(token))
	return err
}
