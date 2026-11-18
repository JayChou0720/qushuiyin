package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

func ID() string { return rand.Text() }
func Digest(key []byte, raw string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

var ErrSession = errors.New("inactive session")

type Store struct {
	DB  *sql.DB
	Key []byte
	TTL time.Duration
}
type Session struct {
	UserID    string    `json:"userId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// DevLogin is only called after the local-only HTTP gate has been checked.
func (s Store) DevLogin(ctx context.Context, name string) (Session, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback()
	subject := Digest(s.Key, "mock user:"+name)
	_, err = tx.ExecContext(ctx, "INSERT INTO app_user(id,app_id,subject_hash,status) VALUES (?,'local',?,'active') ON DUPLICATE KEY UPDATE app_id=app_id", ID(), subject)
	if err != nil {
		return Session{}, err
	}
	var sess Session
	err = tx.QueryRowContext(ctx, "SELECT id FROM app_user WHERE app_id='local' AND subject_hash=? AND status='active'", subject).Scan(&sess.UserID)
	if err != nil {
		return Session{}, err
	}
	sess.Token = ID() + ID()
	sess.ExpiresAt = time.Now().UTC().Add(s.TTL).Truncate(time.Millisecond)
	_, err = tx.ExecContext(ctx, "INSERT INTO app_session(id,user_id,token_digest,session_version,expires_at) VALUES (?,?,?,1,?)", ID(), sess.UserID, s.TokenDigest(sess.Token), sess.ExpiresAt)
	if err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	return sess, nil
}
func (s Store) TokenDigest(token string) string { return Digest(s.Key, "session:"+token) }
func (s Store) User(ctx context.Context, digest string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, "SELECT u.id FROM app_session s JOIN app_user u ON u.id=s.user_id WHERE s.token_digest=? AND s.revoked_at IS NULL AND s.expires_at>UTC_TIMESTAMP(3) AND u.status='active'", digest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSession
	}
	return id, err
}
func (s Store) Revoke(ctx context.Context, digest string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE app_session SET revoked_at=UTC_TIMESTAMP(3) WHERE token_digest=? AND revoked_at IS NULL", digest)
	return err
}
