package httpapi

import "testing"

func TestConfigRolePermissions(t *testing.T) {
	for _, tc := range []struct {
		key                   string
		super, tech, operator bool
	}{
		{"public_notice", true, true, true},
		{"public_help", true, true, true},
		{"max_display_text_chars", true, true, false},
		{"parser_workers", true, true, false},
		{"direct_preview_enabled", true, true, false},
		{"max_request_body_bytes", true, false, false},
		{"global_media_egress_per_day_bytes", true, false, false},
		{"direct_host_review_min_failures", true, true, false},
		{"upstream_challenge_threshold", true, false, false},
		{"admin_login_max_failures", true, false, false},
		{"admin_session_ttl_seconds", true, false, false},
		{"max_image_pixels", true, false, false},
		{"client_event_retention_hours", true, false, false},
		{"tombstone_retention_days", true, false, false},
		{"parser_transient_retries", false, false, false},
		{"backup_retention_days", false, false, false},
	} {
		for role, want := range map[string]bool{"super_admin": tc.super, "tech_admin": tc.tech, "operator": tc.operator, "viewer": false, "support": false, "auditor": false} {
			if got := editableConfig(role, tc.key); got != want {
				t.Errorf("editableConfig(%q, %q) = %t; want %t", role, tc.key, got, want)
			}
		}
	}
}
