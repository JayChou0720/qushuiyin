-- +goose Up
ALTER TABLE save_batch ADD COLUMN direct_reparse_attempt INT UNSIGNED NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE save_batch DROP COLUMN direct_reparse_attempt;
