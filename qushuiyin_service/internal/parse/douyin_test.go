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

const shareFixture = `<script>const example = '<div id=douyin_reflow_token xsstoken=decoy>';</script>
<div xsstoken="fake&amp;token" id=douyin_reflow_token></div>
<input USERcip="region+tag" WebID=0123456789abcdef000 id="douyin_reflow_webId">`

func h5Response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func liveDouyinFixture(t *testing.T, transport coverTransport) Adapter {
	t.Helper()
	l := config.Defaults()
	a := Live(l, ValidationPolicy(), outbound.Client{Redirects: l.Redirects, Transport: transport})
	a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
		t.Fatal("Douyin called the library")
		return nil, nil
	}
	return a
}

func TestDouyinH5VideoAndGallery(t *testing.T) {
	const pageURL = shareOrigin + "/share/video/123/"
	const video = `"video":{"play_addr":{"url_list":["http://cdn.example.test/playwm/v%2F1?sig=playwm%2f+x&sig=2"]},"cover":{"url_list":["http://cdn.example.test/cover?sig=%2f+x"]}}`
	const gallery = `,"images":[{"url_list":["http://cdn.example.test/i%2f1?a=1&a=2+3","https://cdn.example.test/ignored"]},{"url_list":["https://cdn.example.test/i2"]}]`
	for _, test := range []struct {
		name, input, kind string
		images            string
		hops              string
		content           []string
	}{
		{"short video", "https://v.douyin.com/sample/", "video", "", "HHGG", []string{"https://cdn.example.test/play/v%2F1?sig=playwm%2f+x&sig=2"}},
		{"direct mixed gallery", "https://www.douyin.com/note/123", "image", gallery, "GG", []string{"https://cdn.example.test/i%2f1?a=1&a=2+3", "https://cdn.example.test/i2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := 0
			var hops string
			a := liveDouyinFixture(t, func(req *http.Request) (*http.Response, error) {
				hops += req.Method[:1]
				switch {
				case req.URL.Host == "v.douyin.com" && req.Method == http.MethodHead:
					resp := h5Response(302, "")
					resp.Header.Set("Location", "https://www.douyin.com/video/123")
					resp.Header.Set("Set-Cookie", "ttwid=short-link; Path=/")
					return resp, nil
				case req.URL.Host == "www.douyin.com" && req.Method == http.MethodHead:
					return h5Response(200, ""), nil
				case req.URL.String() == pageURL && req.Method == http.MethodGet:
					if req.Header.Get("Cookie") != "" || req.Header.Get("Referer") != "" || !strings.Contains(req.UserAgent(), "Mobile") {
						t.Fatal("unexpected sharing-page headers")
					}
					resp := h5Response(200, shareFixture)
					if run == 0 {
						resp.Header.Add("Set-Cookie", "ttwid=fixture-session; Path=/; Secure; HttpOnly")
						resp.Header.Add("Set-Cookie", "sessionid=fixture-login; Path=/; Secure")
						resp.Header.Add("Set-Cookie", "s_v_web_id=foreign; Domain=example.test; Path=/; Secure")
					}
					return resp, nil
				case req.URL.Host == "www.iesdouyin.com" && req.URL.Path == itemPath && req.Method == http.MethodGet:
					wantCookie := ""
					if run == 0 {
						wantCookie = "ttwid=fixture-session"
					}
					if req.Header.Get("Cookie") != wantCookie || req.Referer() != pageURL || !strings.Contains(req.UserAgent(), "Mobile") {
						t.Fatal("unexpected iteminfo headers")
					}
					q := req.URL.Query()
					for k, want := range map[string]string{
						"reflow_source": "reflow_page", "web_id": "0123456789abcdef000", "device_id": "0123456789abcdef000",
						"use_new_select_scope": "0", "item_ids": "123", "user_cip": "region+tag", "reflow_id": "P/PFgRv/uO/qkzkarq/MNQ==",
					} {
						if q.Get(k) != want {
							t.Errorf("wrong iteminfo query value for %s", k)
						}
					}
					return h5Response(200, `{"status_code":0,"item_list":[{"aweme_id":"123","desc":"example title",`+video+test.images+`}]}`), nil
				default:
					t.Fatalf("unexpected request to %s%s", req.URL.Host, req.URL.Path)
					return nil, nil
				}
			})
			for run = 0; run < 2; run++ {
				hops = ""
				r, err := a.Parse(t.Context(), test.input)
				if err != nil {
					t.Fatal(err)
				}
				if hops != test.hops || r.ContentID == nil || *r.ContentID != "123" || r.MediaType != test.kind || r.Mixed != (test.kind == "image") || len(r.Assets) != len(test.content) {
					t.Fatalf("incorrect result, requests %q, result %+v", hops, r)
				}
				if r.Title == nil || r.Description == nil || *r.Title != "example title" || *r.Description != *r.Title {
					t.Fatal("description was not mapped")
				}
				for i, want := range test.content {
					if r.Assets[i].Source != want || r.Assets[i].Order != i+1 {
						t.Errorf("incorrect content asset %+v", r.Assets[i])
					}
				}
				if r.Cover == nil || r.Cover.Source != "https://cdn.example.test/cover?sig=%2f+x" {
					t.Fatal("incorrect cover")
				}
			}
		})
	}
}

func TestDouyinH5RejectsInvalidUpstream(t *testing.T) {
	for _, test := range []struct {
		name                 string
		page, item, redirect string
		short                bool
		requests             int
		want                 error
	}{
		{"short without ID", "", "", "", true, 1, Unavailable},
		{"short redirects off platform", "", "", "https://douyin.com.example.test/video/123", true, 1, Unavailable},
		{"missing attributes", `<body>`, "", "", false, 1, Unavailable},
		{"item redirects off origin", shareFixture, "", "https://www.douyin.com/video/123", false, 2, Unavailable},
		{"item redirects to other path", shareFixture, "", shareOrigin + "/other", false, 2, Unavailable},
		{"item redirects to HTTP", shareFixture, "", "http://www.iesdouyin.com" + itemPath, false, 2, Unavailable},
		{"nonzero status", shareFixture, `{"status_code":1}`, "", false, 2, Unavailable},
		{"missing status", shareFixture, `{"item_list":[{"aweme_id":"123"}]}`, "", false, 2, InvalidResponse},
		{"mismatched item", shareFixture, `{"status_code":0,"item_list":[{"aweme_id":"456"}]}`, "", false, 2, InvalidResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			a := liveDouyinFixture(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if calls > test.requests {
					t.Fatal("unexpected extra request")
				}
				if (test.short || req.URL.Path == itemPath) && test.redirect != "" {
					resp := h5Response(302, "")
					resp.Header.Set("Location", test.redirect)
					return resp, nil
				}
				if req.URL.Path == itemPath {
					return h5Response(200, test.item), nil
				}
				return h5Response(200, test.page), nil
			})
			input := "https://www.douyin.com/video/123"
			if test.short {
				input = "https://v.douyin.com/sample/"
			}
			if _, err := a.Parse(t.Context(), input); err != test.want || calls != test.requests {
				t.Fatalf("got %v in %d requests, want %v in %d", err, calls, test.want, test.requests)
			}
		})
	}
}
