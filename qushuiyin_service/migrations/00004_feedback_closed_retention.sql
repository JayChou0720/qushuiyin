-- +goose Up
INSERT INTO system_config (id,config_key,value_json,version,published_at,operator_id,reason)
VALUES (REPLACE(UUID(),'-',''),'feedback_closed_retention_days','30',1,UTC_TIMESTAMP(3),'migration','Section 16 and section 19.4');

-- +goose Down
DELETE FROM system_config WHERE config_key='feedback_closed_retention_days' AND version=1 AND operator_id='migration';
