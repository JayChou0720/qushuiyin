package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	_ "github.com/go-sql-driver/mysql"
)

type Domains struct{ DB *sql.DB }

// Observe never changes an operator's decision, even when several probes see
// the host for the first time concurrently.
func (s Domains) Observe(ctx context.Context, p, h string) (string, error) {
	var id [16]byte
	rand.Read(id[:])
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", errors.New("domain store unavailable")
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, `INSERT INTO download_domain
		(id,platform,host,first_seen_at,last_seen_at,created_at,updated_at,seen_count)
		VALUES (?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),1)
		ON DUPLICATE KEY UPDATE last_seen_at=UTC_TIMESTAMP(3),updated_at=UTC_TIMESTAMP(3),
		seen_count=seen_count+1,version=version+1`, hex.EncodeToString(id[:]), p, h)
	if e != nil {
		return "", errors.New("domain observation failed")
	}
	var state string
	if e = tx.QueryRowContext(ctx, "SELECT status FROM download_domain WHERE host=?", h).Scan(&state); e != nil {
		return "", errors.New("domain lookup failed")
	}
	kind := "unregistered"
	if state == "registered" {
		kind = "registered"
	}
	if e = CountOperation(ctx, tx, p, kind, 1); e != nil {
		return "", errors.New("domain observation count failed")
	}
	if e = tx.Commit(); e != nil {
		return "", errors.New("domain commit failed")
	}
	return state, nil
}
