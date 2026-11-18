package media

import (
	"errors"
	"io"
	"net/http"
	"strings"
)

// FixtureHeaders replays the synthetic media metadata without a socket or DB write.
// It is used only by probe in explicit local/test mock mode.
type FixtureHeaders struct{}

func (FixtureHeaders) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "media.example.test" || r.URL.Scheme != "https" {
		return nil, errors.New("missing media fixture")
	}
	kind := "image/jpeg"
	switch r.URL.Path {
	case "/sample.mp4":
		kind = "video/mp4"
	case "/cover.jpg", "/1.jpg", "/2.jpg":
	default:
		return nil, errors.New("missing media fixture")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, ContentLength: 128, Body: io.NopCloser(strings.NewReader(""))}, nil
}
