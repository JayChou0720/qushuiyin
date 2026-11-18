package parse

import (
	"context"
	"errors"
	"github.com/wujunwei928/parse-video/parser"
	"net/http"
	"net/url"
	"os"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
	"strings"
)

const Version = "00e8e118c7c52e287b6b06858639ee86aab0579c"

type Capability struct {
	Status                    string
	Video, Image, Cover, Text bool
}

// ValidationPolicy is only for the local probe CLI. A zero policy opens nothing.
func ValidationPolicy() map[string]Capability {
	m := map[string]Capability{}
	for p := range domains {
		m[p] = Capability{"enabled", true, p != "bilibili", true, true}
	}
	return m
}

type Asset struct {
	Kind, Role string
	Order      int
	Source     string `json:"-"`
}
type Result struct {
	Platform, MediaType                       string
	ContentID, Title, Description, AuthorName *string
	Assets                                    []Asset
	Cover                                     *Asset
	Warnings                                  []string
	Mixed                                     bool
}
type Adapter struct {
	Limits    config.Limits
	Policy    map[string]Capability
	Resolve   func(context.Context, string) (*parser.VideoParseInfo, error)
	Canonical func(context.Context, string, string) *url.URL
}

func Live(l config.Limits, policy map[string]Capability, c outbound.Client) Adapter {
	return Adapter{l, policy, func(ctx context.Context, raw string) (*parser.VideoParseInfo, error) {
		if os.Getenv("PARSE_VIDEO_DOUYIN_COOKIE") != "" {
			return nil, Unavailable
		}
		return parser.ParseVideoShareUrlContext(ctx, raw)
	}, func(ctx context.Context, p, raw string) *url.URL {
		resp, err := c.Headers(ctx, raw, http.MethodHead, "", func(u *url.URL) bool { return Platform(u) == p })
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil
		}
		return resp.Request.URL
	}}
}
func text(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}
func clip(s string, n int) (*string, bool) {
	r := []rune(s)
	if len(r) > n {
		return text(string(r[:n])), true
	}
	return text(s), false
}
func (a Adapter) Parse(ctx context.Context, raw string) (*Result, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, Invalid
	}
	if !outbound.HTTPS(u) {
		return nil, Forbidden
	}
	p := Platform(u)
	cap := a.Policy[p]
	if cap.Status == "maintenance" {
		return nil, Code("PLATFORM_MAINTENANCE")
	}
	if p == "" || cap.Status != "enabled" {
		return nil, Unsupported
	}
	if p == "bilibili" && !biliAllowed(u) {
		return nil, TypeUnsupported
	}
	ctx, stop := context.WithTimeout(ctx, a.Limits.Job)
	defer stop()
	canonical := u
	if contentID(p, u) == nil {
		short, done := context.WithTimeout(ctx, a.Limits.ParserCall)
		canonical = a.Canonical(short, p, raw)
		done()
	}
	// Bilibili's library always parses the first part. Reject links whose
	// requested part could not be confirmed before calling the library.
	if p == "bilibili" && (contentID(p, canonical) == nil || !biliAllowed(canonical)) {
		return nil, TypeUnsupported
	}
	call, done := context.WithTimeout(ctx, a.Limits.ParserCall)
	defer done()
	info, err := a.Resolve(call, raw)
	if errors.Is(call.Err(), context.Canceled) {
		return nil, Canceled
	}
	if errors.Is(call.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return nil, Timeout
	}
	if err != nil {
		return nil, Unavailable
	}
	if info == nil {
		return nil, InvalidResponse
	}
	r := &Result{Platform: p, ContentID: contentID(p, canonical), Warnings: []string{}, AuthorName: text(info.Author.Name), Mixed: len(info.Images) > 0 && info.VideoUrl != ""}
	if cap.Text {
		var t, d bool
		r.Title, t = clip(info.Title, a.Limits.Title)
		r.Description, d = clip(info.Title, a.Limits.Description)
		if t || d {
			r.Warnings = append(r.Warnings, "TEXT_TRUNCATED")
		}
	}
	asset := func(raw, kind, role string, i int) (Asset, error) {
		u, e := url.Parse(raw)
		if e != nil || !outbound.HTTPS(u) {
			return Asset{}, InvalidResponse
		}
		return Asset{kind, role, i, raw}, nil
	}
	if len(info.Images) > 0 {
		if !cap.Image || p == "bilibili" {
			return nil, TypeUnsupported
		}
		if len(info.Images) > a.Limits.Assets {
			return nil, Code("TOO_MANY_ASSETS")
		}
		r.MediaType = "image"
		live := false
		for i, img := range info.Images {
			x, e := asset(img.Url, "image", "content", i+1)
			if e != nil {
				return nil, e
			}
			r.Assets = append(r.Assets, x)
			live = live || img.LivePhotoUrl != ""
		}
		if live {
			r.Warnings = append(r.Warnings, "LIVE_PHOTO_AS_STATIC")
		}
	} else {
		if info.VideoUrl == "" {
			return nil, InvalidResponse
		}
		if !cap.Video {
			return nil, TypeUnsupported
		}
		r.MediaType = "video"
		x, e := asset(info.VideoUrl, "video", "content", 1)
		if e != nil {
			return nil, e
		}
		r.Assets = []Asset{x}
	}
	if cap.Cover && info.CoverUrl != "" {
		x, e := asset(info.CoverUrl, "image", "cover", 1)
		if e != nil {
			return nil, e
		}
		r.Cover = &x
	}
	return r, nil
}
