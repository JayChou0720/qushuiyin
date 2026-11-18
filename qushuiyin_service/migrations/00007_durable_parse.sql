-- +goose Up
ALTER TABLE parse_job ADD COLUMN expires_at DATETIME(3), ADD COLUMN force_refresh BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE parse_job SET expires_at=deadline_at WHERE expires_at IS NULL;
ALTER TABLE parse_job MODIFY expires_at DATETIME(3) NOT NULL;
ALTER TABLE parse_result ADD COLUMN history_url_enc TEXT;

-- +goose Down
ALTER TABLE parse_result DROP COLUMN history_url_enc;
ALTER TABLE parse_job DROP COLUMN force_refresh, DROP COLUMN expires_at;
