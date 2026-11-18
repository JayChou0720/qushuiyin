package parse

import (
	"context"
	"encoding/json"
	"github.com/wujunwei928/parse-video/parser"
	"net/url"
	"qushuiyin/service/internal/config"
	fixtures "qushuiyin/service/testdata/parse_fixtures"
	"strings"
)

const MockVersion = "fixture-v1"

// MockSourceKey uses the canonical work URL when the fixture has a work ID.
// Short links are never expanded by Mock, so their original URLs are used.
func MockSourceKey(platform string, u *url.URL, raw string) string {
	if contentID(platform, u) == nil {
		return raw
	}
	normalized := *u
	normalized.Host = strings.ToLower(u.Host)
	normalized.Fragment, normalized.RawFragment = "", ""
	return normalized.String()
}

func Mock(l config.Limits, policy map[string]Capability) (Adapter, error) {
	var samples map[string]struct {
		Failure bool
		Info    *parser.VideoParseInfo
	}
	if err := json.Unmarshal(fixtures.Samples, &samples); err != nil {
		return Adapter{}, err
	}
	return Adapter{Limits: l, Policy: policy, Resolve: func(_ context.Context, raw string) (*parser.VideoParseInfo, error) {
		s, ok := samples[raw]
		if !ok || s.Failure {
			return nil, Unavailable
		}
		return s.Info, nil
	}, Canonical: func(context.Context, string, string) (*url.URL, error) { return nil, nil }}, nil
}
