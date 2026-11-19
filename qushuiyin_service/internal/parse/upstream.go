package parse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"qushuiyin/service/internal/config"
)

type upstreamChallenge struct{}

func (upstreamChallenge) Error() string { return Unavailable.Error() }
func (upstreamChallenge) Unwrap() error { return Unavailable }

// ErrUpstreamChallenge exposes the same public code as other unavailable responses.
var ErrUpstreamChallenge error = upstreamChallenge{}

type upstreamGuard struct {
	mu        sync.Mutex
	policy    config.Upstream
	now       func() time.Time
	platforms map[string]*upstreamState
}

type upstreamState struct {
	requests, challenges       []time.Time
	samples, failures, library []time.Time
	cooldown                   time.Duration
	openUntil                  time.Time
	halfOpen                   bool
	epoch                      uint64
}

type upstreamPermit struct {
	guard       *upstreamGuard
	state       *upstreamState
	platform    string
	epoch       uint64
	probe, used bool
}

func newUpstreamGuard(policy config.Upstream) *upstreamGuard {
	return &upstreamGuard{policy: policy, now: time.Now, platforms: make(map[string]*upstreamState)}
}

func (a Adapter) CircuitState(platform string) string {
	if a.guard == nil {
		return "unavailable"
	}
	g := a.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.platforms[platform]
	if state == nil || state.openUntil.IsZero() {
		return "closed"
	}
	if g.now().Before(state.openUntil) {
		return "open"
	}
	return "half_open"
}

func (g *upstreamGuard) begin(platform string) (*upstreamPermit, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.platforms[platform]
	if state == nil {
		state = &upstreamState{}
		g.platforms[platform] = state
	}
	if state.halfOpen || g.now().Before(state.openUntil) {
		return nil, Maintenance
	}
	probe := !state.openUntil.IsZero()
	state.halfOpen = probe
	return &upstreamPermit{guard: g, state: state, platform: platform, epoch: state.epoch, probe: probe}, nil
}

func expireTimes(times []time.Time, cutoff time.Time) []time.Time {
	for len(times) > 0 && !times[0].After(cutoff) {
		times = times[1:]
	}
	return times
}

func (p *upstreamPermit) wait(ctx context.Context) error {
	g, s := p.guard, p.state
	for {
		if err := ctx.Err(); err != nil {
			if err == context.Canceled {
				return Canceled
			}
			return Busy
		}
		g.mu.Lock()
		now := g.now()
		if p.epoch != s.epoch {
			g.mu.Unlock()
			return Maintenance
		}
		s.requests = expireTimes(s.requests, now.Add(-time.Minute))
		if len(s.requests) < g.policy.RequestsPerMinute {
			s.requests = append(s.requests, now)
			p.used = true
			g.mu.Unlock()
			return nil
		}
		wake := s.requests[0].Add(time.Minute)
		g.mu.Unlock()
		if deadline, ok := ctx.Deadline(); ok && !wake.Before(deadline) {
			return Busy
		}
		timer := time.NewTimer(wake.Sub(now))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
}

func (p *upstreamPermit) finish(err error, circuit config.Circuit) {
	g, s := p.guard, p.state
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.epoch != s.epoch {
		return
	}
	now := g.now()
	challenge := errors.Is(err, ErrUpstreamChallenge)
	sourceFailure := errors.Is(err, Timeout) || errors.Is(err, InvalidResponse)
	libraryFailure := errors.Is(err, Unavailable)
	cooldown, reason := circuit.Open, "source_failure"
	if p.probe {
		s.halfOpen = false
		if !p.used {
			return
		}
		if err == nil {
			s.openUntil = time.Time{}
			s.challenges, s.samples, s.failures, s.library = nil, nil, nil, nil
			return
		}
		if !challenge && !sourceFailure && !libraryFailure {
			return
		}
		cooldown, reason = s.cooldown, "probe_failed"
	}
	if challenge {
		s.challenges = append(expireTimes(s.challenges, now.Add(-g.policy.ChallengeWindow)), now)
		if !p.probe && len(s.challenges) < g.policy.ChallengeThreshold {
			return
		}
		cooldown, reason = g.policy.ChallengeCooldown, "verification_page"
	} else if !p.probe {
		if !p.used || circuit.MinSamples <= 0 || circuit.Window <= 0 || circuit.Open <= 0 ||
			err != nil && !sourceFailure && !libraryFailure {
			return
		}
		cutoff := now.Add(-circuit.Window)
		s.samples = append(expireTimes(s.samples, cutoff), now)
		s.failures, s.library = expireTimes(s.failures, cutoff), expireTimes(s.library, cutoff)
		if sourceFailure {
			s.failures = append(s.failures, now)
		}
		if libraryFailure {
			s.library = append(s.library, now)
		}
		total := int64(len(s.samples))
		if total < circuit.MinSamples {
			return
		}
		if len(s.library) > 0 && int64(len(s.library))*100 >= total*circuit.LibraryPercent {
			reason = "library_error"
		} else if len(s.failures) == 0 || int64(len(s.failures))*100 < total*circuit.FailurePercent {
			return
		}
	}
	s.cooldown = cooldown
	s.openUntil = now.Add(cooldown)
	s.epoch++
	slog.Warn("upstream platform cooldown", "event", "upstream_circuit_open", "platform", p.platform, "reason", reason, "until", s.openUntil.UTC())
}

type upstreamPermitKey struct{}

type upstreamTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

type upstreamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b upstreamBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

func (t upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if permit, ok := r.Context().Value(upstreamPermitKey{}).(*upstreamPermit); ok {
		if err := permit.wait(r.Context()); err != nil {
			return nil, err
		}
	}
	// Start the I/O timeout after admission, so time queued uses only the job budget.
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	resp, err := t.base.RoundTrip(r.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = upstreamBody{resp.Body, cancel}
	return resp, nil
}

func upstreamProtectionError(err error) error {
	for _, code := range []error{Busy, Maintenance, Canceled, ErrUpstreamChallenge} {
		if errors.Is(err, code) {
			return code
		}
	}
	return nil
}
