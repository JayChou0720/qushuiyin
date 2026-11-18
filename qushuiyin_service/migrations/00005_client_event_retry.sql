-- +goose Up
INSERT INTO system_config (id,config_key,value_json,version,published_at,operator_id,reason) VALUES
  (REPLACE(UUID(),'-',''),'client_event_retry_initial_seconds','5',1,UTC_TIMESTAMP(3),'migration','Section 16 client event retry'),
  (REPLACE(UUID(),'-',''),'client_event_retry_max_seconds','300',1,UTC_TIMESTAMP(3),'migration','Section 16 client event retry');

-- +goose Down
DELETE FROM system_config WHERE config_key IN ('client_event_retry_initial_seconds','client_event_retry_max_seconds') AND version=1 AND operator_id='migration';
