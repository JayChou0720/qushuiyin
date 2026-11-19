-- +goose NO TRANSACTION
-- +goose Up
CREATE TABLE ops_minute_metric (
  bucket_at DATETIME NOT NULL,
  platform VARCHAR(32) NOT NULL,
  kind VARCHAR(32) NOT NULL,
  count BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY(bucket_at,platform,kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
ALTER TABLE parse_job ADD INDEX ix_parse_job_expiry (expires_at);
ALTER TABLE usage_counter ADD INDEX ix_usage_day (stat_date);
INSERT INTO system_config(id,config_key,value_json,version,published_at,operator_id,reason)
VALUES (REPLACE(UUID(),'-',''),'media_egress_alert_percent','90',1,UTC_TIMESTAMP(3),'migration','Section 16 egress alert');

-- +goose Down
DELETE FROM system_config WHERE config_key='media_egress_alert_percent';
ALTER TABLE usage_counter DROP INDEX ix_usage_day;
ALTER TABLE parse_job DROP INDEX ix_parse_job_expiry;
DROP TABLE ops_minute_metric;
