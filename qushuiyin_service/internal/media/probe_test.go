package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"strings"
	"testing"
)

func TestProbeSniffsGenericMediaTypes(t *testing.T) {
	for _, tc := range []struct {
		name, kind, head, get, prefix, mime, reason string
		status, reads                               int
		badRange                                    bool
	}{
		{name: "JPEG after generic HEAD", kind: "image", head: "application/octet-stream", get: "application/octet-stream", prefix: "\xff\xd8\xff", mime: "image/jpeg", reason: "OK", status: 206, reads: 12},
		{name: "PNG with no type and Range ignored", kind: "image", prefix: "\x89PNG\r\n\x1a\n", mime: "image/png", reason: "OK", status: 200, reads: 12},
		{name: "MP4 after HEAD 405", kind: "video", head: "405", get: "binary/octet-stream", prefix: "\x00\x00\x00\x18ftypmp42", mime: "video/mp4", reason: "OK", status: 206, reads: 12},
		{name: "WebP requires conversion", kind: "image", get: "binary/octet-stream", prefix: "RIFF\x00\x00\x00\x00WEBP", mime: "image/webp", reason: "UNSUPPORTED_MIME", status: 206, reads: 12},
		{name: "unknown format", kind: "image", prefix: "not an image", reason: "UNSUPPORTED_MIME", status: 206, reads: 12},
		{name: "explicit HTML does not trigger sniffing", kind: "image", head: "text/html", mime: "text/html", reason: "UNSUPPORTED_MIME"},
		{name: "MP4 does not pass as an image", kind: "image", prefix: "\x00\x00\x00\x18ftypmp42", mime: "video/mp4", reason: "UNSUPPORTED_MIME", status: 206, reads: 12},
		{name: "partial body must start at zero", kind: "image", prefix: "\xff\xd8\xff", reason: "INVALID_SIZE", status: 206, badRange: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := config.Defaults()
			l.RangeBytes = 16
			body := &countedBody{Reader: strings.NewReader(tc.prefix + strings.Repeat("0", 200))}
			hops := 0
			client := outbound.Client{Transport: round(func(req *http.Request) (*http.Response, error) {
				hops++
				headers := make(http.Header)
				if req.Method == http.MethodHead {
					status := http.StatusOK
					if tc.head == "405" {
						status = http.StatusMethodNotAllowed
					} else if tc.head != "" {
						headers.Set("Content-Type", tc.head)
					}
					return &http.Response{StatusCode: status, Header: headers, ContentLength: 200, Body: http.NoBody}, nil
				}
				if req.Method != http.MethodGet || req.Header.Get("Range") != "bytes=0-15" || tc.status == 0 {
					t.Fatal("unexpected Range request")
				}
				if tc.get != "" {
					headers.Set("Content-Type", tc.get)
				}
				length := int64(200)
				if tc.status == http.StatusPartialContent {
					start := 0
					if tc.badRange {
						start = 1
					}
					headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/200", start, start+15))
					length = 16
				}
				return &http.Response{StatusCode: tc.status, Header: headers, ContentLength: length, Body: body}, nil
			})}
			obs := (Probe{client, l, &record{status: "registered"}}).Inspect(t.Context(), "xiaohongshu", parse.Asset{Kind: tc.kind, Source: "https://cdn.test/media"})
			if obs.Type != tc.mime || obs.Reason != tc.reason || obs.Direct != (tc.reason == "OK") || body.reads != tc.reads {
				t.Fatalf("observation %+v, read %d bytes", obs, body.reads)
			}
			if tc.status == 0 && hops != 1 || tc.status != 0 && (hops != 2 || !body.closed) {
				t.Fatalf("%d requests, Range body closed %v", hops, body.closed)
			}
			if obs.Direct && (obs.Size == nil || *obs.Size != 200) {
				t.Fatal("size must refer to the entire file")
			}
		})
	}
}

type countedBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.reads += n
	return n, err
}

func (b *countedBody) Close() error { b.closed = true; return nil }

func TestProbeTriesBoundedAlternatesForOneAsset(t *testing.T) {
	l := config.Defaults()
	a := parse.Asset{Kind: "image", Role: "content", Order: 4, Source: "https://cdn.test/primary.jpg", Alternates: []string{"https://cdn.test/one.webp", "https://cdn.test/two.jpeg"}}
	for _, limit := range []int{2, 3} {
		l.MediaCandidates = limit
		calls := 0
		client := outbound.Client{Transport: round(func(r *http.Request) (*http.Response, error) {
			calls++
			typ := "image/webp"
			if r.URL.Path == "/two.jpeg" {
				typ = "image/jpeg"
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {typ}}, ContentLength: 42, Body: http.NoBody}, nil
		})}
		obs := (Probe{client, l, &record{status: "registered"}}).Inspect(t.Context(), "douyin", a)
		if calls != limit || obs.Order != a.Order || obs.Role != a.Role || obs.Direct != (limit == 3) {
			t.Fatalf("limit %d, calls %d, observation %+v", limit, calls, obs)
		}
	}
}

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
