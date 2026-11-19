package httpapi

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"time"

	"qushuiyin/service/internal/auth"
)

const deletionManifestContext = "qushuiyin deletion manifest v1:"

type deletionMarker struct {
	UserID    string    `json:"userId"`
	DeletedAt time.Time `json:"deletedAt"`
}

type deletionManifest struct {
	Version int             `json:"version"`
	Markers json.RawMessage `json:"markers"`
	MAC     string          `json:"mac"`
}

// ExportDeletions reads the authoritative database, independently of the backup
// that will be restored.
func ExportDeletions(ctx context.Context, db *sql.DB, key []byte, out io.Writer) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,deleted_at FROM app_user WHERE status='deleted' ORDER BY id")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	markers := []deletionMarker{}
	for rows.Next() {
		var marker deletionMarker
		if err = rows.Scan(&marker.UserID, &marker.DeletedAt); err != nil {
			return 0, err
		}
		markers = append(markers, marker)
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	data, err := json.Marshal(markers)
	if err != nil {
		return 0, err
	}
	err = json.NewEncoder(out).Encode(deletionManifest{
		Version: 1, Markers: data, MAC: auth.Digest(key, deletionManifestContext+string(data)),
	})
	return len(markers), err
}

// ReplayDeletions is an offline restore operation. Each user is purged
// transactionally and the same manifest can be replayed after interruption.
func ReplayDeletions(ctx context.Context, db *sql.DB, key []byte, in io.Reader) (int, error) {
	var manifest deletionManifest
	dec := json.NewDecoder(in)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil || dec.Decode(new(any)) != io.EOF || manifest.Version != 1 ||
		!hmac.Equal([]byte(manifest.MAC), []byte(auth.Digest(key, deletionManifestContext+string(manifest.Markers)))) {
		return 0, errors.New("invalid deletion manifest")
	}
	var markers []deletionMarker
	if err := json.Unmarshal(manifest.Markers, &markers); err != nil || markers == nil {
		return 0, errors.New("invalid deletion markers")
	}
	for _, marker := range markers {
		if !opaqueID(marker.UserID) || marker.DeletedAt.Year() < 1000 || marker.DeletedAt.Year() > 9999 {
			return 0, errors.New("invalid deletion marker")
		}
	}
	for i, marker := range markers {
		if err := replayDeletion(ctx, db, marker); err != nil {
			return i, err
		}
	}
	return len(markers), nil
}

func replayDeletion(ctx context.Context, db *sql.DB, marker deletionMarker) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO app_user(id,app_id,status,deleted_at)
		VALUES (?,'restore-tombstone','deleted',?) AS incoming
		ON DUPLICATE KEY UPDATE status='deleted',subject_hash=NULL,
			deleted_at=LEAST(COALESCE(app_user.deleted_at,incoming.deleted_at),incoming.deleted_at)`, marker.UserID, marker.DeletedAt.UTC())
	if err != nil {
		return err
	}
	if err = purgeUserRows(ctx, tx, marker.UserID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE cleanup_job SET status='SUCCEEDED',attempt=attempt+1,last_error_code=NULL
		WHERE user_id=? AND kind='user_data' AND status<>'SUCCEEDED'`, marker.UserID); err != nil {
		return err
	}
	return tx.Commit()
}
