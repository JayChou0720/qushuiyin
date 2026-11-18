-- +goose Up
INSERT INTO system_config (id,config_key,value_json,version,published_at,operator_id,reason) VALUES
  (REPLACE(UUID(),'-',''),'upstream_requests_per_minute','20',1,UTC_TIMESTAMP(3),'migration','Section 16.4'),
  (REPLACE(UUID(),'-',''),'upstream_challenge_threshold','3',1,UTC_TIMESTAMP(3),'migration','Section 16.4'),
  (REPLACE(UUID(),'-',''),'upstream_challenge_cooldown_seconds','600',1,UTC_TIMESTAMP(3),'migration','Section 16.4');

-- +goose Down
DELETE FROM system_config WHERE config_key IN ('upstream_requests_per_minute','upstream_challenge_threshold','upstream_challenge_cooldown_seconds') AND version=1 AND operator_id='migration';
