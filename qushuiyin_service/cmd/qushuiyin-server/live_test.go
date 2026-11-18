//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"qushuiyin/service/internal/parse"
)

// Opt in with -tags live. Do not log URLs, even when a sample fails.
func TestLiveSamples(t *testing.T) {
	b, e := os.ReadFile("../../testdata/live_samples.local.json")
	if errors.Is(e, os.ErrNotExist) {
		t.Skip("no local live samples")
	}
	if e != nil {
		t.Fatal("cannot read sample file")
	}
	var samples []struct {
		URL, Platform, MediaType, Error string
		ProbeOnly                       bool
	}
	if json.Unmarshal(b, &samples) != nil || len(samples) == 0 {
		t.Fatal("invalid sample file")
	}
	const batchLimit = 10
	const platformInterval = 10 * time.Second
	lastFinished := make(map[string]time.Time)
	stopped := make(map[string]bool)
	attempted := 0
	var adapter parse.Adapter
	order := make([]int, 0, len(samples))
	for _, probeOnly := range []bool{false, true} {
		for i, s := range samples {
			if s.ProbeOnly == probeOnly {
				order = append(order, i)
			}
		}
	}
	for _, i := range order {
		s := samples[i]
		if attempted >= batchLimit {
			t.Log("live batch reached 10 samples")
			break
		}
		u, _ := url.Parse(s.URL)
		platform := parse.Platform(u)
		if stopped[platform] {
			continue
		}
		if wait := time.Until(lastFinished[platform].Add(platformInterval)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-t.Context().Done():
				timer.Stop()
				return
			}
		}
		attempted++
		var out bytes.Buffer
		e := runWithProbeAdapter(t.Context(), []string{"probe", s.URL}, func(k string) string {
			switch k {
			case "APP_ENV":
				return "test"
			case "PARSE_MODE":
				return "live"
			case "PARSE_VIDEO_DOUYIN_COOKIE":
				return os.Getenv(k)
			}
			return ""
		}, &out, &adapter)
		lastFinished[platform] = time.Now()
		if errors.Is(e, parse.ErrUpstreamChallenge) {
			stopped[platform] = true
			t.Logf("sample %d (%s): verification page, stopped platform for this run; wait at least 30 minutes before retesting", i+1, platform)
			continue
		}
		// Previously observed public samples may expire. Keep them available
		// for reproduction without asserting that their upstream status is fixed.
		if s.ProbeOnly {
			if e != nil {
				t.Logf("sample %d (probe only): %v", i+1, e)
			} else {
				t.Logf("sample %d (probe only): %s", i+1, out.String())
			}
			continue
		}
		if e != nil {
			if e.Error() != s.Error {
				t.Errorf("sample %d: %v", i+1, e)
			}
			continue
		}
		var rep report
		if s.Error != "" || json.Unmarshal(out.Bytes(), &rep) != nil || rep.Platform != s.Platform || rep.Kind != s.MediaType || rep.Count < 1 {
			t.Errorf("sample %d: unexpected result", i+1)
		}
		t.Logf("sample %d: %s", i+1, out.String())
	}
}
