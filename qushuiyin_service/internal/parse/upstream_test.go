package parse

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wujunwei928/parse-video/parser"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
)

func TestCircuitSeparatesFailureCategories(t *testing.T) {
	now := time.Now()
	g := newUpstreamGuard(config.Defaults().Upstream)
	g.now = func() time.Time { return now }
	policy := config.Circuit{Window: 5 * time.Minute, Open: time.Minute, MinSamples: 4, FailurePercent: 50, LibraryPercent: 80}
	record := func(platform string, err error) {
		t.Helper()
		permit, acquireErr := g.begin(platform)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		permit.used = true
		permit.finish(err, policy)
	}
	record("douyin", nil)
	record("douyin", Timeout)
	record("douyin", Unavailable)
	record("douyin", nil)
	if state := (Adapter{guard: g}).CircuitState("douyin"); state != "closed" {
		t.Fatal("different error categories must not share a numerator")
	}
	record("douyin", InvalidResponse)
	record("douyin", Timeout)
	if _, err := g.begin("douyin"); !errors.Is(err, Maintenance) {
		t.Fatal("expected the configured source error circuit to open")
	}
	record("kuaishou", nil)
	now = now.Add(time.Minute)
	a := Adapter{guard: g, Policy: map[string]Capability{"douyin": {Status: "maintenance"}}}
	if _, err := a.Parse(t.Context(), "https://www.douyin.com/video/10001"); !errors.Is(err, Maintenance) {
		t.Fatal("manual maintenance must reject an automatic probe")
	}
	record("douyin", nil)
	if state := a.CircuitState("douyin"); state != "closed" {
		t.Fatal("a healthy probe did not recover the circuit")
	}
}

func TestPlatformLimitCountsRedirectAndLibraryTogether(t *testing.T) {
	limits := config.Defaults()
	limits.Upstream.RequestsPerMinute = 2
	hops, library := 0, 0
	a := Live(limits, ValidationPolicy(), outbound.Client{Redirects: limits.Redirects, Transport: coverTransport(func(r *http.Request) (*http.Response, error) {
		hops++
		if r.URL.Host == "xhslink.com" {
			res := h5Response(302, "")
			res.Header.Set("Location", "https://www.xiaohongshu.com/explore/abc")
			return res, nil
		}
		return h5Response(200, ""), nil
	})})
	now := time.Now()
	a.guard.now = func() time.Time { return now }
	a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
		library++
		return &parser.VideoParseInfo{VideoUrl: "https://cdn.example.test/video.mp4"}, nil
	}
	if _, err := a.Parse(t.Context(), "https://xhslink.com/abc"); !errors.Is(err, Busy) || hops != 2 || library != 0 {
		t.Fatalf("wanted two hops and a busy response before the library call, got %d/%d %v", hops, library, err)
	}
	if _, err := a.Parse(t.Context(), "https://www.bilibili.com/video/BV123"); err != nil || library != 1 {
		t.Fatalf("Bilibili should have its own quota: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err := a.Parse(t.Context(), "https://www.xiaohongshu.com/explore/abc"); err != nil || library != 2 {
		t.Fatalf("quota did not renew: %v", err)
	}
}

func TestDouyinChallengeEnablesCooldown(t *testing.T) {
	calls, good := 0, false
	a := liveDouyinFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if !good {
			return h5Response(200, `<script>WAFJS('_wafchallengeid')</script>`), nil
		}
		if r.URL.Path == itemPath {
			return h5Response(200, `{"status_code":0,"item_list":[{"aweme_id":"123","video":{"play_addr":{"url_list":["https://cdn.example.test/video.mp4"]}}}]}`), nil
		}
		return h5Response(200, shareFixture), nil
	})
	now := time.Now()
	a.guard.now = func() time.Time { return now }
	const raw = "https://www.douyin.com/video/123"
	for range a.Limits.Upstream.ChallengeThreshold {
		if _, err := a.Parse(t.Context(), raw); !errors.Is(err, ErrUpstreamChallenge) || !errors.Is(err, Unavailable) {
			t.Fatalf("verification page must map to UPSTREAM_UNAVAILABLE: %v", err)
		}
	}
	if _, err := a.Parse(t.Context(), raw); !errors.Is(err, Maintenance) || calls != 3 {
		t.Fatalf("cooldown must block all outgoing requests; calls %d, error %v", calls, err)
	}
	now = now.Add(a.Limits.Upstream.ChallengeCooldown)
	if _, err := a.Parse(t.Context(), raw); !errors.Is(err, ErrUpstreamChallenge) {
		t.Fatalf("failed probe: %v", err)
	}
	if _, err := a.Parse(t.Context(), raw); !errors.Is(err, Maintenance) || calls != 4 {
		t.Fatalf("failed probe must renew cooldown; calls %d, error %v", calls, err)
	}
	now = now.Add(a.Limits.Upstream.ChallengeCooldown)
	good = true
	for range 2 {
		if _, err := a.Parse(t.Context(), raw); err != nil {
			t.Fatalf("platform should recover after the first successful probe: %v", err)
		}
	}
	if calls != 8 {
		t.Fatalf("expected two requests for each successful parse, got %d total", calls)
	}
}

func TestCooldownAllowsOneProbe(t *testing.T) {
	p := config.Defaults().Upstream
	p.ChallengeThreshold = 1
	g := newUpstreamGuard(p)
	now := time.Now()
	g.now = func() time.Time { return now }
	first, _ := g.begin("douyin")
	first.finish(ErrUpstreamChallenge, config.Circuit{})
	now = now.Add(p.ChallengeCooldown)
	probe, err := g.begin("douyin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.begin("douyin"); !errors.Is(err, Maintenance) {
		t.Fatalf("second probe admitted: %v", err)
	}
	probe.finish(Canceled, config.Circuit{})
	if _, err = g.begin("douyin"); err != nil {
		t.Fatalf("unused probe should release the slot: %v", err)
	}
}

func TestLiveProbeStopsOnFirstIncompleteSharePage(t *testing.T) {
	calls := 0
	a := liveDouyinFixture(t, func(r *http.Request) (*http.Response, error) {
		calls++
		return h5Response(200, "<body></body>"), nil
	})
	_, err := a.Parse(StopOnVerificationPage(t.Context()), "https://www.douyin.com/video/123")
	if !errors.Is(err, ErrUpstreamChallenge) || calls != 1 {
		t.Fatalf("live probe should stop before a second request: %d requests, %v", calls, err)
	}
}

func TestUpstreamWaitUsesRemainingJobBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := config.Defaults()
		limits.Upstream.RequestsPerMinute = 1
		a := Live(limits, ValidationPolicy(), outbound.Client{})
		a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
			return &parser.VideoParseInfo{VideoUrl: "https://cdn.example.test/video.mp4"}, nil
		}
		const link = "https://www.bilibili.com/video/BV123"
		if _, err := a.Parse(t.Context(), link); err != nil {
			t.Fatal(err)
		}
		time.Sleep(45 * time.Second)
		started := time.Now()
		if _, err := a.Parse(t.Context(), link); err != nil {
			t.Fatalf("slot should become available before the job deadline: %v", err)
		}
		if waited := time.Since(started); waited != 15*time.Second {
			t.Fatalf("expected the rolling minute to end after 15 seconds, waited %s", waited)
		}
	})
}
