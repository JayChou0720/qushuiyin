package outbound

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestDialPinsPublicAddress(t *testing.T) {
	pub := netip.MustParseAddr("93.184.216.34")
	for _, bad := range []string{"127.0.0.1", "10.0.0.1", "::ffff:192.168.1.1", "::1", "169.254.169.254", "100.64.0.1", "198.18.0.1", "2001:db8::1", ""} {
		called := false
		dial := checkedDial(func(context.Context, string, string) ([]netip.Addr, error) {
			ips := []netip.Addr{pub}
			if bad != "" {
				ips = append(ips, netip.MustParseAddr(bad))
			}
			return ips, nil
		}, func(_ context.Context, _, addr string) (net.Conn, error) {
			called = true
			if addr != "93.184.216.34:443" {
				t.Fatal("not pinned")
			}
			return nil, nil
		})
		_, e := dial(t.Context(), "tcp", "cdn.test:443")
		if (e == nil) != (bad == "") || called != (bad == "") {
			t.Fatal(bad, e)
		}
	}
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRedirects(t *testing.T) {
	for _, tc := range []struct {
		loc        string
		max, calls int
		ok         bool
	}{{"/end", 1, 2, true}, {"/end", 0, 1, false}, {"/start", 4, 1, false}, {"https://other.test/", 3, 1, false}} {
		calls := 0
		c := Client{transport(func(r *http.Request) (*http.Response, error) {
			calls++
			s := 302
			if r.URL.Path == "/end" {
				s = 200
			}
			return &http.Response{StatusCode: s, Header: http.Header{"Location": {tc.loc}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}), tc.max}
		r, e := c.Headers(t.Context(), "https://source.test/start", "HEAD", "", func(u *url.URL) bool { return HTTPS(u) && u.Host == "source.test" })
		if r != nil {
			r.Body.Close()
		}
		if (e == nil) != tc.ok || calls != tc.calls {
			t.Fatal(tc, e)
		}
	}
}
