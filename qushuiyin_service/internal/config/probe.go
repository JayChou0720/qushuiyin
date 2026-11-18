// Package config owns the M0 subset of spec section 16.
package config

import "time"

// Upstream contains the server-side platform limits from section 16.4.
type Upstream struct {
	RequestsPerMinute, ChallengeThreshold int
	ChallengeWindow, ChallengeCooldown    time.Duration
}

func (v Values) Upstream() (Upstream, error) {
	if err := v.Validate("upstream_requests_per_minute", "upstream_challenge_threshold", "upstream_challenge_cooldown_seconds", "circuit_window_seconds"); err != nil {
		return Upstream{}, err
	}
	return Upstream{int(v.Int("upstream_requests_per_minute")), int(v.Int("upstream_challenge_threshold")), v.Seconds("circuit_window_seconds"), v.Seconds("upstream_challenge_cooldown_seconds")}, nil
}

type Limits struct {
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
