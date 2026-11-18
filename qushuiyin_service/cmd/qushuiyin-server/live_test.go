//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
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
	for i, s := range samples {
		var out bytes.Buffer
		e := run(t.Context(), []string{"probe", s.URL}, func(k string) string {
			switch k {
			case "APP_ENV":
				return "test"
			case "PARSE_MODE":
				return "live"
			case "PARSE_VIDEO_DOUYIN_COOKIE":
				return os.Getenv(k)
			}
			return ""
		}, &out)
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
