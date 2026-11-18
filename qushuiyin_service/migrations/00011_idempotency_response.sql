-- +goose Up
ALTER TABLE idempotency_record ADD COLUMN response_json JSON NULL;

-- +goose Down
ALTER TABLE idempotency_record DROP COLUMN response_json;
