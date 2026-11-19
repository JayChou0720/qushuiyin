package parse

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/wujunwei928/parse-video/parser"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
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
	Source     string   `json:"-"`
	Alternates []string `json:"-"`
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
	Limits      config.Limits
	Policy      map[string]Capability
	Resolve     func(context.Context, string) (*parser.VideoParseInfo, error)
	Canonical   func(context.Context, string, string) (*url.URL, error)
	guard       *upstreamGuard
	douyin      func(context.Context, string) (*h5Item, error)
	coverClient outbound.Client
}

func Live(l config.Limits, policy map[string]Capability, c outbound.Client) Adapter {
	protected := c
	protected.Transport = upstreamTransport{base: c.Transport, timeout: l.ParserCall}
	return Adapter{
		Limits: l,
		guard:  newUpstreamGuard(l.Upstream),
		Policy: policy,
		Resolve: func(ctx context.Context, raw string) (*parser.VideoParseInfo, error) {
			if os.Getenv("PARSE_VIDEO_DOUYIN_COOKIE") != "" {
				return nil, Unavailable
			}
			return parser.ParseVideoShareUrlContext(ctx, raw)
		},
		Canonical: func(ctx context.Context, p, raw string) (*url.URL, error) {
			resp, err := protected.Headers(ctx, raw, http.MethodHead, "", func(u *url.URL) bool {
				if p == "douyin" {
					return h5Host(u)
				}
				return Platform(u) == p
			})
			if err != nil {
				return nil, upstreamProtectionError(err)
			}
			defer resp.Body.Close()
			if p == "douyin" && contentID(p, resp.Request.URL) != nil {
				// Short-link expansion only needs the work ID. The terminal
				// public page can reject HEAD while H5 iteminfo still works.
				return resp.Request.URL, nil
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, nil
			}
			return resp.Request.URL, nil
		},
		douyin: func(ctx context.Context, id string) (*h5Item, error) {
			return douyinItem(ctx, protected, id)
		},
		coverClient: c,
	}
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

func httpsMediaSource(raw string) string {
	if strings.HasPrefix(strings.ToLower(raw), "http://") {
		raw = "https" + raw[len("http"):]
	}
	if u, err := url.Parse(raw); err != nil || !outbound.HTTPS(u) {
		return ""
	}
	return raw
}

func (a Adapter) Parse(ctx context.Context, raw string) (_ *Result, retErr error) {
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
		return nil, Maintenance
	}
	if p == "" || cap.Status != "enabled" {
		return nil, Unsupported
	}
	if p == "bilibili" && !biliAllowed(u) {
		return nil, TypeUnsupported
	}
	ctx, stop := context.WithTimeout(ctx, a.Limits.Job)
	defer stop()
	var permit *upstreamPermit
	if a.guard != nil {
		permit, err = a.guard.begin(p)
		if err != nil {
			return nil, err
		}
		ctx = context.WithValue(ctx, upstreamPermitKey{}, permit)
		defer func() { permit.finish(retErr, a.Limits.Circuit) }()
	}
	canonical := u
	if contentID(p, u) == nil {
		canonical, err = a.Canonical(ctx, p, raw)
		if err != nil {
			return nil, err
		}
	}
	// Bilibili's library always parses the first part. Reject links whose
	// requested part could not be confirmed before calling the library.
	if p == "bilibili" && (contentID(p, canonical) == nil || !biliAllowed(canonical)) {
		return nil, TypeUnsupported
	}
	if permit != nil && p != "douyin" {
		// The pinned library owns its transport, so reserve once per library call.
		if err := permit.wait(ctx); err != nil {
			return nil, err
		}
	}
	call := ctx
	if p != "douyin" || a.douyin == nil {
		var done context.CancelFunc
		call, done = context.WithTimeout(ctx, a.Limits.ParserCall)
		defer done()
	}
	var info *parser.VideoParseInfo
	var imageAlternates [][]string
	if p == "douyin" && a.douyin != nil {
		work := contentID(p, canonical)
		if work == nil {
			err = Unavailable
		} else {
			var item *h5Item
			item, err = a.douyin(call, *work)
			if item != nil {
				info, imageAlternates = item.info, item.imageAlternates
			}
		}
	} else {
		info, err = a.Resolve(call, raw)
	}
	if protected := upstreamProtectionError(err); protected != nil {
		return nil, protected
	}
	if errors.Is(call.Err(), context.Canceled) {
		return nil, Canceled
	}
	if errors.Is(call.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return nil, Timeout
	}
	if errors.Is(err, Timeout) {
		return nil, Timeout
	}
	if errors.Is(err, InvalidResponse) {
		return nil, InvalidResponse
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
		source := httpsMediaSource(raw)
		if source == "" {
			return Asset{}, InvalidResponse
		}
		return Asset{Kind: kind, Role: role, Order: i, Source: source}, nil
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
			if i < len(imageAlternates) {
				seen := map[string]bool{x.Source: true}
				for _, raw := range imageAlternates[i] {
					alternate := httpsMediaSource(raw)
					if alternate != "" && !seen[alternate] {
						seen[alternate] = true
						x.Alternates = append(x.Alternates, alternate)
					}
				}
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
		source := info.CoverUrl
		if p == "bilibili" {
			source = a.bilibiliCover(call, source)
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil, Canceled
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, Timeout
			}
			if source == "" {
				return r, nil
			}
		}
		x, e := asset(source, "image", "cover", 1)
		if e != nil {
			return nil, e
		}
		r.Cover = &x
	}
	return r, nil
}
