-- +goose NO TRANSACTION
-- +goose Up
INSERT INTO system_config(id,config_key,value_json,version,published_at,operator_id,reason) VALUES
  (REPLACE(UUID(),'-',''),'public_notice',JSON_QUOTE(''),1,UTC_TIMESTAMP(3),'migration','Section 17 public display'),
  (REPLACE(UUID(),'-',''),'public_help',JSON_QUOTE(''),1,UTC_TIMESTAMP(3),'migration','Section 17 public display');

ALTER TABLE parse_job ADD COLUMN metrics_recorded BOOLEAN NOT NULL DEFAULT FALSE, ADD INDEX ix_parse_finished_at (finished_at);

CREATE TABLE operation_sample (
  id VARCHAR(64) NOT NULL PRIMARY KEY,
  created_at DATETIME(3) NOT NULL,
  user_id VARCHAR(64) NOT NULL,
  trace_id VARCHAR(64) NOT NULL,
  platform VARCHAR(32) NOT NULL,
  adapter_version VARCHAR(100) NOT NULL,
  stage VARCHAR(32) NOT NULL,
  outcome VARCHAR(32) NOT NULL,
  source_kind VARCHAR(20) NOT NULL,
  delivery_mode VARCHAR(16) NOT NULL,
  error_code VARCHAR(64),
  duration_ms BIGINT UNSIGNED NOT NULL,
  bytes BIGINT UNSIGNED NOT NULL,
  INDEX(created_at,id),
  INDEX(trace_id),
  INDEX(user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

-- +goose Down
DROP TABLE operation_sample;
ALTER TABLE parse_job DROP INDEX ix_parse_finished_at, DROP COLUMN metrics_recorded;
DELETE FROM system_config WHERE config_key IN ('public_notice','public_help');
