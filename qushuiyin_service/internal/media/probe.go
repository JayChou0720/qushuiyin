package media

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	ctx, cancel := context.WithTimeout(ctx, p.Limits.Media)
	defer cancel()
	best := p.inspectSource(ctx, platform, a, a.Source)
	for i, source := range a.Alternates {
		if best.Direct || best.Reason == "DOMAIN_STORE_FAILED" || (p.Domains == nil && best.Reason == "DOMAIN_NOT_REGISTERED") || i+1 >= p.Limits.MediaCandidates || ctx.Err() != nil {
			break
		}
		obs := p.inspectSource(ctx, platform, a, source)
		if obs.Direct || obs.Reason == "DOMAIN_STORE_FAILED" {
			return obs
		}
		if best.Reason != "DOMAIN_NOT_REGISTERED" && obs.Reason == "DOMAIN_NOT_REGISTERED" {
			best = obs
		}
	}
	return best
}

func contentType(resp *http.Response) (string, bool) {
	header := strings.TrimSpace(resp.Header.Get("Content-Type"))
	typ, _, _ := mime.ParseMediaType(header)
	return typ, header == "" || typ == "application/octet-stream" || typ == "binary/octet-stream"
}

func successStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusPartialContent
}

// The longest signature ends at byte 12. Small and unbounded responses alike
// are read only up to that prefix, and never beyond the configured Range.
func sniffMedia(r io.Reader, rangeBytes int64) string {
	prefix, err := io.ReadAll(io.LimitReader(r, min(rangeBytes, 12)))
	if err != nil {
		return ""
	}
	switch {
	case bytes.HasPrefix(prefix, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(prefix, []byte("\x89PNG")):
		return "image/png"
	case len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "image/webp"
	case len(prefix) >= 8 && string(prefix[4:8]) == "ftyp":
		return "video/mp4"
	}
	return ""
}

func (p Probe) inspectSource(ctx context.Context, platform string, a parse.Asset, source string) Observation {
	o := Observation{Order: a.Order, Role: a.Role, Domain: "untracked", Reason: "PROBE_FAILED"}
	allow := func(u *url.URL) bool { return outbound.HTTPS(u) }
	resp, e := p.Client.Headers(ctx, source, "HEAD", "", allow)
	if e != nil {
		return o
	}
	mt, generic := contentType(resp)
	sniff := successStatus(resp.StatusCode) && generic
	if sniff || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		raw := resp.Request.URL.String()
		resp.Body.Close()
		if p.Limits.RangeBytes <= 0 {
			return o
		}
		resp, e = p.Client.Headers(ctx, raw, "GET", fmt.Sprintf("bytes=0-%d", p.Limits.RangeBytes-1), allow)
		if e != nil {
			return o
		}
		mt, generic = contentType(resp)
		sniff = sniff || generic
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
	if !successStatus(resp.StatusCode) {
		o.Reason = "HTTP_STATUS"
		return o
	}
	var valid bool
	o.Size, valid = size(resp)
	if !valid {
		o.Reason = "INVALID_SIZE"
		return o
	}
	if sniff {
		// The validated Range starts at byte zero.
		mt = sniffMedia(resp.Body, p.Limits.RangeBytes)
	}
	if len(mt) <= 100 {
		o.Type = mt
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
