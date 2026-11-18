-- +goose Up
CREATE TABLE download_domain (
 id VARCHAR(64) PRIMARY KEY,
 platform VARCHAR(32) NOT NULL, host VARCHAR(253) CHARACTER SET ascii COLLATE ascii_general_ci NOT NULL,
 status ENUM('observed','registered','ignored','review') NOT NULL DEFAULT 'observed',
 first_seen_at DATETIME(3) NOT NULL, last_seen_at DATETIME(3) NOT NULL,
 created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
 seen_count BIGINT NOT NULL, direct_issued_total BIGINT NOT NULL DEFAULT 0,
 direct_fallback_total BIGINT NOT NULL DEFAULT 0, registered_at DATETIME(3),
 operator_id VARCHAR(64), note VARCHAR(200), version INT UNSIGNED NOT NULL DEFAULT 1,
 UNIQUE KEY uq_download_domain_host (host), KEY ix_download_domain_platform_status (platform,status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
-- +goose Down
DROP TABLE download_domain;
