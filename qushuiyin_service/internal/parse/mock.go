package parse

import (
	"context"
	"encoding/json"
	"github.com/wujunwei928/parse-video/parser"
	"net/url"
	"qushuiyin/service/internal/config"
	fixtures "qushuiyin/service/testdata/parse_fixtures"
)

func Mock(l config.Limits, policy map[string]Capability) (Adapter, error) {
	var samples map[string]struct {
		Failure bool
		Info    *parser.VideoParseInfo
	}
	if err := json.Unmarshal(fixtures.Samples, &samples); err != nil {
		return Adapter{}, err
	}
	return Adapter{l, policy, func(_ context.Context, raw string) (*parser.VideoParseInfo, error) {
		s, ok := samples[raw]
		if !ok || s.Failure {
			return nil, Unavailable
		}
		return s.Info, nil
	}, func(context.Context, string, string) *url.URL { return nil }}, nil
}
