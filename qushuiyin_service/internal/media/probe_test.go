package media

import (
	"context"
	"io"
	"net/http"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"strings"
	"testing"
)

type round func(*http.Request) (*http.Response, error)

func (f round) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type record struct {
	status, host string
	count        int
}

func TestUnknownSizeCannotUseDirect(t *testing.T) {
	l := config.Defaults()
	d := &record{status: "registered"}
	c := outbound.Client{Redirects: l.Redirects, Transport: round(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	o := (Probe{c, l, d}).Inspect(t.Context(), "douyin", parse.Asset{Kind: "video", Source: "https://cdn.example.test/video"})
	if o.Direct || o.Reason != "SIZE_UNKNOWN" || o.Size != nil {
		t.Fatal(o)
	}
}

func (d *record) Observe(_ context.Context, _ string, h string) (string, error) {
	d.host = h
	d.count++
	return d.status, nil
}
func TestFinalHostAndRange(t *testing.T) {
	l := config.Defaults()
	for _, tc := range []struct{ state, p, mime, cr, reason string }{{"registered", "douyin", "video/mp4", "bytes 0-1/200", "OK"}, {"review", "douyin", "video/mp4", "bytes 0-1/200", "DOMAIN_NOT_REGISTERED"}, {"registered", "bilibili", "video/mp4", "bytes 0-1/200", "RELAY_ONLY"}, {"registered", "douyin", "video/mp4", "bytes 0-1/200000000", "INVALID_SIZE"}, {"registered", "douyin", "video/mp4", "", "INVALID_SIZE"}, {"registered", "douyin", "text/html", "bytes 0-1/200", "UNSUPPORTED_MIME"}} {
		d := &record{status: tc.state}
		c := outbound.Client{Redirects: l.Redirects, Transport: round(func(r *http.Request) (*http.Response, error) {
			s := 206
			h := http.Header{"Content-Type": {tc.mime}, "Content-Range": {tc.cr}}
			if r.URL.Host == "source.test" {
				s = 302
				h.Set("Location", "https://final.test/v?sig=hidden")
			} else if r.Method == "HEAD" {
				s = 405
			} else if r.Header.Get("Range") != "bytes=0-4095" || r.Header.Get("Referer") != "" {
				t.Fatal("unexpected headers")
			}
			return &http.Response{StatusCode: s, Header: h, Body: io.NopCloser(strings.NewReader("")), ContentLength: 2}, nil
		})}
		o := (Probe{c, l, d}).Inspect(t.Context(), tc.p, parse.Asset{Kind: "video", Source: "https://source.test/v"})
		if o.Reason != tc.reason || o.Direct != (tc.reason == "OK") || d.host != "final.test" || d.count != 1 {
			t.Fatal(o, d)
		}
		if o.Direct && (o.Size == nil || *o.Size != 200) {
			t.Fatal("partial length treated as total")
		}
	}
}
