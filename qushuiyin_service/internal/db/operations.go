package db

import (
	"context"
	"database/sql"
)

func CountOperation(ctx context.Context, tx *sql.Tx, platform, kind string, count int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO ops_minute_metric(bucket_at,platform,kind,count)
		VALUES (DATE_FORMAT(UTC_TIMESTAMP(),'%Y-%m-%d %H:%i:00'),?,?,?) AS sample
		ON DUPLICATE KEY UPDATE count=ops_minute_metric.count+sample.count`, platform, kind, count)
	return err
}
