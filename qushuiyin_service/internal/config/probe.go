// Package config owns the M0 subset of spec section 16.
package config

import "time"

// Upstream contains the server-side platform limits from section 16.4.
type Upstream struct {
	RequestsPerMinute, ChallengeThreshold int
	ChallengeWindow, ChallengeCooldown    time.Duration
}

func (v Values) Upstream() (Upstream, error) {
	if err := v.Validate("upstream_requests_per_minute", "upstream_challenge_threshold", "upstream_challenge_cooldown_seconds", "upstream_challenge_window_seconds"); err != nil {
		return Upstream{}, err
	}
	return Upstream{int(v.Int("upstream_requests_per_minute")), int(v.Int("upstream_challenge_threshold")), v.Seconds("upstream_challenge_window_seconds"), v.Seconds("upstream_challenge_cooldown_seconds")}, nil
}

type Circuit struct {
	Window, Open                               time.Duration
	MinSamples, FailurePercent, LibraryPercent int64
}

func (v Values) Circuit() Circuit {
	return Circuit{v.Seconds("circuit_window_seconds"), v.Seconds("circuit_open_seconds"),
		v.Int("circuit_min_samples"), v.Int("circuit_failure_percent"), v.Int("circuit_library_error_percent")}
}

type Limits struct {
	Circuit                                                     Circuit
	Upstream                                                    Upstream
	Redirects, Assets, Title, Description, MediaCandidates      int
	RangeBytes, VideoBytes, ImageBytes                          int64
	Job, ParserCall, Media, Batch, AssumedTTL, Download, Margin time.Duration
}

func Defaults() Limits {
	return Limits{
		Upstream:  Upstream{20, 3, 5 * time.Minute, 10 * time.Minute},
		Redirects: 5, Assets: 100, Title: 200, Description: 10000, MediaCandidates: 3,
		RangeBytes: 4096, VideoBytes: 104857600, ImageBytes: 20971520,
		Job: 25 * time.Second, ParserCall: 10 * time.Second, Media: 90 * time.Second, Batch: 180 * time.Second,
		AssumedTTL: 600 * time.Second, Download: 120 * time.Second, Margin: 30 * time.Second,
	}
}

// Limits maps the runtime settings shared by parsing and media probing.
func (v Values) Limits() Limits {
	return Limits{
		Circuit:   v.Circuit(),
		Redirects: int(v.Int("max_redirects")), Assets: int(v.Int("max_result_assets")),
		Title: int(v.Int("max_title_chars")), Description: int(v.Int("max_description_chars")),
		MediaCandidates: int(v.Int("max_media_candidates")), RangeBytes: v.Int("direct_probe_range_bytes"),
		VideoBytes: v.Int("max_video_bytes"), ImageBytes: v.Int("max_image_bytes"),
		Job: v.Seconds("parse_job_timeout_seconds"), ParserCall: v.Seconds("parser_call_timeout_seconds"),
		Media: v.Seconds("media_fetch_timeout_seconds"), Batch: v.Seconds("batch_prepare_timeout_seconds"),
		AssumedTTL: v.Seconds("direct_url_assumed_ttl_seconds"), Download: v.Seconds("client_download_timeout_seconds"),
		Margin: v.Seconds("ticket_safety_margin_seconds"),
	}
}
