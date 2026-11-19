-- +goose Up
INSERT INTO system_config(id,config_key,value_json,version,published_at,operator_id,reason)
VALUES (REPLACE(UUID(),'-',''),'max_display_text_chars','2000',1,UTC_TIMESTAMP(3),'migration','Section 17 display text limit');

-- +goose Down
DELETE FROM system_config WHERE config_key='max_display_text_chars';
