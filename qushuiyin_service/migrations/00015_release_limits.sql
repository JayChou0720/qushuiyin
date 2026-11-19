-- +goose Up
INSERT INTO system_config(id,config_key,value_json,version,published_at,operator_id,reason) VALUES
(REPLACE(UUID(),'-',''),'database_max_open_connections','16',1,UTC_TIMESTAMP(3),'migration','Section 16 release limits'),
(REPLACE(UUID(),'-',''),'wechat_login_timeout_seconds','10',1,UTC_TIMESTAMP(3),'migration','Section 16 release limits'),
(REPLACE(UUID(),'-',''),'app_login_requests_per_minute','600',1,UTC_TIMESTAMP(3),'migration','Section 16 release limits'),
(REPLACE(UUID(),'-',''),'ip_public_config_requests_per_minute','120',1,UTC_TIMESTAMP(3),'migration','Section 16 release limits'),
(REPLACE(UUID(),'-',''),'user_poll_requests_per_minute','120',1,UTC_TIMESTAMP(3),'migration','Section 16 release limits');

-- +goose Down
DELETE FROM system_config WHERE config_key IN ('database_max_open_connections','wechat_login_timeout_seconds',
'app_login_requests_per_minute','ip_public_config_requests_per_minute','user_poll_requests_per_minute');
