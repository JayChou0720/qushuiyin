// Package outbound handles the application's own header-only requests. The upstream
// parser keeps its independent transport and policy.
package outbound

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

var denied = errors.New("target not allowed")
var label = regexp.MustCompile("^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")

func HTTPS(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || strings.HasSuffix(u.Host, ":") || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return false
	}
	for _, s := range strings.Split(h, ".") {
		if !label.MatchString(s) {
			return false
		}
	}
	return true
}

var special = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Fields("0.0.0.0/8 100.64.0.0/10 192.0.0.0/24 192.0.2.0/24 192.31.196.0/24 192.52.193.0/24 192.88.99.0/24 192.175.48.0/24 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 240.0.0.0/4 2001::/23 2001:db8::/32 2002::/16 2620:4f:8000::/48 3fff::/20") {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()
var globalV6 = netip.MustParsePrefix("2000::/3")

func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalV6.Contains(ip)) {
		return false
	}
	for _, p := range special {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialFunc func(context.Context, string, string) (net.Conn, error)

func checkedDial(lookup lookupFunc, dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, denied
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, denied
		}
		for _, ip := range ips {
			if !public(ip) {
				return nil, denied
			}
		}
		for _, ip := range ips {
			var c net.Conn
			c, err = dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return c, nil
			}
		}
		return nil, denied
	}
}

// Requests never inherit proxy settings, cookies, Referer or credentials. DNS
// is checked inside DialContext and the socket connects to that exact IP.
func Transport() *http.Transport {
	return &http.Transport{DialContext: checkedDial(net.DefaultResolver.LookupNetIP, (&net.Dialer{}).DialContext), DisableKeepAlives: true, DisableCompression: true}
}

type Client struct {
	Transport http.RoundTripper
	Redirects int
}

func (c Client) Headers(ctx context.Context, raw, method, byteRange string, allow func(*url.URL) bool) (*http.Response, error) {
	u, err := url.Parse(raw)
	seen := map[string]bool{}
	for hops := 0; err == nil && hops <= c.Redirects; hops++ {
		if !allow(u) || seen[u.String()] {
			break
		}
		seen[u.String()] = true
		req, e := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if e != nil {
			break
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		resp, e := c.Transport.RoundTrip(req)
		if e != nil {
			return nil, e
		}
		resp.Request = req
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			u, err = resp.Location()
			resp.Body.Close()
		default:
			return resp, nil
		}
	}
	return nil, denied
}
