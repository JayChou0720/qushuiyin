package parse

import (
	"context"
	"errors"
	"fmt"
	"github.com/wujunwei928/parse-video/parser"
	"net/url"
	"qushuiyin/service/internal/config"
	"testing"
)

func TestGalleryAssetLimitKeepsOriginalOrder(t *testing.T) {
	l := config.Defaults()
	a, err := Mock(l, ValidationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 20, 21, l.Assets, l.Assets + 1} {
		info := &parser.VideoParseInfo{}
		for order := 1; order <= count; order++ {
			info.Images = append(info.Images, parser.ImgInfo{Url: fmt.Sprintf("https://media.example.test/%d.jpg", order)})
		}
		a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) { return info, nil }
		result, err := a.Parse(t.Context(), "https://www.xiaohongshu.com/explore/10004")
		if count > l.Assets {
			if err != Code("TOO_MANY_ASSETS") {
				t.Fatalf("gallery above the limit returned %v", err)
			}
			continue
		}
		if err != nil || len(result.Assets) != count {
			t.Fatalf("gallery of %d did not survive normalization: %v", count, err)
		}
		for index, asset := range result.Assets {
			if asset.Order != index+1 || asset.Source != info.Images[index].Url {
				t.Fatal("gallery order changed", asset.Order)
			}
		}
	}
}

func TestFixtures(t *testing.T) {
	a, e := Mock(config.Defaults(), ValidationPolicy())
	if e != nil {
		t.Fatal(e)
	}
	for p, base := range map[string]string{"douyin": "https://www.douyin.com/video/", "kuaishou": "https://www.kuaishou.com/short-video/", "xiaohongshu": "https://www.xiaohongshu.com/explore/", "bilibili": "https://www.bilibili.com/video/BV"} {
		t.Run(p, func(t *testing.T) {
			v, e := a.Parse(t.Context(), base+"10001")
			if e != nil || v.Platform != p || v.MediaType != "video" || len(v.Assets) != 1 || v.Cover == nil {
				t.Fatalf("video: %v", e)
			}
			im, e := a.Parse(t.Context(), base+"10002")
			if p == "bilibili" {
				if e != TypeUnsupported {
					t.Fatal(e)
				}
			} else if e != nil || !im.Mixed || len(im.Assets) != 2 || im.Assets[1].Order != 2 || im.Warnings[0] != "LIVE_PHOTO_AS_STATIC" {
				t.Fatalf("image: %v", e)
			}
			_, e = a.Parse(t.Context(), base+"10003")
			if e != Unavailable {
				t.Fatal(e)
			}
		})
	}
}
func TestShortLinksAndGates(t *testing.T) {
	a, _ := Mock(config.Defaults(), ValidationPolicy())
	calls := 0
	raw := "https://xhslink.cn/abc?token=keep"
	a.Resolve = func(ctx context.Context, s string) (*parser.VideoParseInfo, error) {
		calls++
		if s != raw {
			t.Fatal("original URL changed")
		}
		return &parser.VideoParseInfo{VideoUrl: "https://cdn.example.test/v"}, nil
	}
	for _, dest := range []string{"", "https://www.xiaohongshu.com/explore/123abc"} {
		a.Canonical = func(context.Context, string, string) (*url.URL, error) {
			if dest == "" {
				return nil, nil
			}
			u, _ := url.Parse(dest)
			return u, nil
		}
		r, e := a.Parse(t.Context(), raw)
		if e != nil || r.Title != nil || r.Cover != nil || (dest == "") != (r.ContentID == nil) {
			t.Fatal(e)
		}
		if dest != "" && *r.ContentID != "123abc" {
			t.Fatal("content id")
		}
	}
	for _, s := range []string{"https://www.douyin.com.invalid.test/", "https://www.douyin.com:8080/", "https://bad@www.douyin.com/", "https://127.0.0.1/", "https://www.bilibili.com/video/BV123?p=2", "https://www.bilibili.com/bangumi/play/1", "https://www.bilibili.com/cheese/play/1"} {
		if _, e := a.Parse(t.Context(), s); e == nil {
			t.Fatal("gate admitted URL")
		}
	}
	a.Policy = nil
	if _, e := a.Parse(t.Context(), raw); e != Unsupported || calls != 2 {
		t.Fatal(e, calls)
	}
}
func TestErrorMapping(t *testing.T) {
	a, _ := Mock(config.Defaults(), ValidationPolicy())
	raw := "https://www.douyin.com/video/123"
	for _, tc := range []struct {
		info             *parser.VideoParseInfo
		cause, errorWant error
	}{{nil, nil, InvalidResponse}, {&parser.VideoParseInfo{}, nil, InvalidResponse}, {&parser.VideoParseInfo{VideoUrl: "ftp://x.test/v"}, nil, InvalidResponse}, {nil, errors.New("private source text"), Unavailable}, {nil, context.DeadlineExceeded, Timeout}} {
		a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) { return tc.info, tc.cause }
		if _, e := a.Parse(t.Context(), raw); e != tc.errorWant {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := a.Parse(ctx, raw); e != Canceled {
		t.Fatal(e)
	}
}

func TestXiaohongshuMediaHTTPSUpgrade(t *testing.T) {
	a, err := Mock(config.Defaults(), ValidationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	const share = "https://www.xiaohongshu.com/explore/123abc"
	const source = "http://cdn.example.test/a%2fb?sig=%2f+x&sig=2"
	const upgraded = "https://cdn.example.test/a%2fb?sig=%2f+x&sig=2"
	for _, info := range []*parser.VideoParseInfo{
		{VideoUrl: source, CoverUrl: source},
		{Images: []parser.ImgInfo{{Url: source}}},
	} {
		a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) { return info, nil }
		r, err := a.Parse(t.Context(), share)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Assets) != 1 || r.Assets[0].Source != upgraded || (info.CoverUrl != "" && (r.Cover == nil || r.Cover.Source != upgraded)) {
			t.Fatal("HTTPS upgrade changed the path or signed query")
		}
	}
	for _, raw := range []string{"http://user@cdn.example.test/v", "http://cdn.example.test:80/v", "http://127.0.0.1/v"} {
		a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
			return &parser.VideoParseInfo{VideoUrl: raw}, nil
		}
		if _, err := a.Parse(t.Context(), share); err != InvalidResponse {
			t.Fatalf("unsafe media URL passed or produced the wrong error: %v", err)
		}
	}
}

func TestBilibiliShortLinkPart(t *testing.T) {
	a, _ := Mock(config.Defaults(), ValidationPolicy())
	calls := 0
	a.Resolve = func(context.Context, string) (*parser.VideoParseInfo, error) {
		calls++
		return &parser.VideoParseInfo{VideoUrl: "https://cdn.example.test/v"}, nil
	}
	for _, tc := range []struct {
		target string
		calls  int
		want   error
	}{
		{"", 0, TypeUnsupported},
		{"https://b23.tv/still-short", 0, TypeUnsupported},
		{"https://www.bilibili.com/video/BV123?p=2", 0, TypeUnsupported},
		{"https://www.bilibili.com/video/BV123?p=1", 1, nil},
	} {
		a.Canonical = func(context.Context, string, string) (*url.URL, error) {
			if tc.target == "" {
				return nil, nil
			}
			u, _ := url.Parse(tc.target)
			return u, nil
		}
		_, e := a.Parse(t.Context(), "https://b23.tv/short")
		if e != tc.want || calls != tc.calls {
			t.Fatalf("target=%q error=%v calls=%d", tc.target, e, calls)
		}
	}
}
