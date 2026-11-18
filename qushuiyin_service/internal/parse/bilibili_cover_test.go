package parse

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wujunwei928/parse-video/parser"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
)

type coverTransport func(*http.Request) (*http.Response, error)

func (f coverTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBilibiliOptionalCover(t *testing.T) {
	const httpCover = "http://i2.hdslb.com/bfs/archive/a.png?sig=keep%2Bvalue"
	const httpsCover = "https://i2.hdslb.com/bfs/archive/a.png?sig=keep%2Bvalue"
	const video = "https://cdn.example.test/video.mp4"
	for _, tc := range []struct {
		name, origin, location, kind, want string
		status, calls                      int
	}{
		{"verified upgrade", httpCover, "", "image/png", httpsCover, 200, 1},
		{"existing HTTPS", httpsCover, "", "", httpsCover, 0, 0},
		{"other host", "http://i2.hdslb.com.example.test/bfs/archive/a.png", "", "", "", 0, 0},
		{"unexpected path", "http://i2.hdslb.com/other/a.png", "", "", "", 0, 0},
		{"credentials", "http://name@i2.hdslb.com/bfs/archive/a.png", "", "", "", 0, 0},
		{"unexpected port", "http://i2.hdslb.com:8080/bfs/archive/a.png", "", "", "", 0, 0},
		{"downgrade", httpCover, httpCover, "", "", 302, 1},
		{"redirect to other host", httpCover, "https://other.example.test/cover.png", "", "", 302, 1},
		{"not an image", httpCover, "", "text/html", "", 200, 1},
		{"HTTP failure", httpCover, "", "image/png", "", 404, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := config.Defaults()
			calls := 0
			client := outbound.Client{Redirects: l.Redirects, Transport: coverTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodHead || r.URL.String() != httpsCover || calls > 1 {
					t.Fatal("unexpected outbound cover request")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.kind}, "Location": {tc.location}}, ContentLength: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			a := Live(l, ValidationPolicy(), client)
			a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
				return &parser.VideoParseInfo{VideoUrl: video, CoverUrl: tc.origin}, nil
			}
			r, err := a.Parse(t.Context(), "https://www.bilibili.com/video/BV123?p=1")
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Assets) != 1 || r.Assets[0].Source != video || calls != tc.calls {
				t.Fatalf("unexpected video result or probe count: %d", calls)
			}
			if tc.want == "" {
				if r.Cover != nil {
					t.Fatal("unverified cover retained")
				}
				return
			}
			if r.Cover == nil || r.Cover.Source != tc.want {
				t.Fatal("expected cover missing or modified")
			}
		})
	}
}
