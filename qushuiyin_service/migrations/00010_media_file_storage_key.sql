-- +goose Up
ALTER TABLE media_file ADD INDEX idx_media_file_storage_key (storage_key);

-- +goose Down
ALTER TABLE media_file DROP INDEX idx_media_file_storage_key;
