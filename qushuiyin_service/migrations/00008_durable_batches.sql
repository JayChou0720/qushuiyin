-- +goose Up
ALTER TABLE save_batch ADD COLUMN state_enc MEDIUMTEXT;

-- +goose Down
ALTER TABLE save_batch DROP COLUMN state_enc;
