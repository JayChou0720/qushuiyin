-- +goose Up
INSERT INTO system_config (id,config_key,value_json,version,published_at,operator_id,reason) VALUES
  (REPLACE(UUID(),'-',''),'http_read_timeout_seconds','25',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'http_write_timeout_seconds','25',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'http_idle_timeout_seconds','60',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'database_operation_timeout_seconds','25',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'admin_password_workers','4',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'ip_login_requests_per_minute','60',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'ip_admin_login_requests_per_minute','60',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'memory_mock_max_entries','10000',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'memory_media_ticket_max_entries','10000',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'cleanup_batch_size','50',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'ffprobe_max_output_bytes','4096',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits'),
  (REPLACE(UUID(),'-',''),'upstream_challenge_window_seconds','300',1,UTC_TIMESTAMP(3),'migration','Section 16 independent limits');

-- +goose Down
DELETE FROM system_config WHERE operator_id='migration' AND version=1 AND reason='Section 16 independent limits';
