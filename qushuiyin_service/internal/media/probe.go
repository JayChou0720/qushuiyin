package media

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
	"regexp"
	"strconv"
	"strings"
)

type Domains interface {
	Observe(context.Context, string, string) (string, error)
}
type Observation struct {
	Order  int    `json:"order"`
	Role   string `json:"role"`
	Host   string `json:"finalHost,omitempty"`
	Type   string `json:"contentType,omitempty"`
	Size   *int64 `json:"sizeBytes"`
	Domain string `json:"domainStatus"`
	Direct bool   `json:"directEligible"`
	Reason string `json:"reason"`
}
type Probe struct {
	Client  outbound.Client
	Limits  config.Limits
	Domains Domains
}

var partial = regexp.MustCompile("^bytes ([0-9]+)-([0-9]+)/([0-9]+)$")

func size(resp *http.Response) (*int64, bool) {
	n := resp.ContentLength
	if resp.StatusCode == 206 {
		m := partial.FindStringSubmatch(resp.Header.Get("Content-Range"))
		if m == nil {
			return nil, false
		}
		a, e1 := strconv.ParseInt(m[1], 10, 64)
		b, e2 := strconv.ParseInt(m[2], 10, 64)
		total, e3 := strconv.ParseInt(m[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || a != 0 || b < a || b >= total || (n >= 0 && n != b-a+1) {
			return nil, false
		}
		n = total
	}
	if n < 0 {
		return nil, true
	}
	return &n, true
}
func (p Probe) Inspect(ctx context.Context, platform string, a parse.Asset) Observation {
	o := Observation{Order: a.Order, Role: a.Role, Domain: "untracked", Reason: "PROBE_FAILED"}
	ctx, cancel := context.WithTimeout(ctx, p.Limits.Media)
	defer cancel()
	allow := func(u *url.URL) bool { return outbound.HTTPS(u) }
	resp, e := p.Client.Headers(ctx, a.Source, "HEAD", "", allow)
	if e != nil {
		return o
	}
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		raw := resp.Request.URL.String()
		resp.Body.Close()
		resp, e = p.Client.Headers(ctx, raw, "GET", fmt.Sprintf("bytes=0-%d", p.Limits.RangeBytes-1), allow)
		if e != nil {
			return o
		}
	}
	defer resp.Body.Close()
	o.Host = strings.ToLower(resp.Request.URL.Hostname())
	if p.Domains != nil {
		o.Domain, e = p.Domains.Observe(ctx, platform, o.Host)
		if e != nil {
			o.Domain = "unknown"
			o.Reason = "DOMAIN_STORE_FAILED"
			return o
		}
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		o.Reason = "HTTP_STATUS"
		return o
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if len(mt) <= 100 {
		o.Type = mt
	}
	var valid bool
	o.Size, valid = size(resp)
	if !valid {
		o.Reason = "INVALID_SIZE"
		return o
	}
	limit := p.Limits.ImageBytes
	ok := mt == "image/jpeg" || mt == "image/png"
	if a.Kind == "video" {
		limit = p.Limits.VideoBytes
		ok = strings.HasPrefix(mt, "video/")
	}
	switch {
	case !ok:
		o.Reason = "UNSUPPORTED_MIME"
	case o.Size == nil:
		o.Reason = "SIZE_UNKNOWN"
	case *o.Size == 0 || *o.Size > limit:
		o.Reason = "INVALID_SIZE"
	case platform == "bilibili" && a.Kind == "video":
		o.Reason = "RELAY_ONLY"
	case o.Domain != "registered":
		o.Reason = "DOMAIN_NOT_REGISTERED"
	case p.Limits.AssumedTTL < p.Limits.Download+p.Limits.Margin:
		o.Reason = "TTL_TOO_SHORT"
	default:
		o.Direct = true
		o.Reason = "OK"
	}
	return o
}
