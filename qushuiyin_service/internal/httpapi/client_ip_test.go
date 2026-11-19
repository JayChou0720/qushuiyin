package httpapi

import (
	"net/http/httptest"
	"testing"

	"qushuiyin/service/internal/config"
)

func TestClientIP(t *testing.T) {
	proxies, err := config.TrustedProxyCIDRs("127.0.0.1/32, 10.0.0.0/24, ::1/128")
	if err != nil {
		t.Fatal(err)
	}
	s := Mock{TrustedProxies: proxies}
	for _, tc := range []struct {
		name, peer, forwarded, real, want string
	}{
		{"direct", "192.0.2.5:1234", "198.51.100.3", "198.51.100.4", "192.0.2.5"},
		{"single proxy", "127.0.0.1:1234", "198.51.100.3", "", "198.51.100.3"},
		{"trusted chain", "127.0.0.1:1234", "192.0.2.5, 198.51.100.3, 10.0.0.2", "", "198.51.100.3"},
		{"real IPv6", "[::1]:1234", "", "2001:db8::7", "2001:db8::7"},
		{"invalid hop", "127.0.0.1:1234", "192.0.2.5, invalid, 10.0.0.2", "198.51.100.3", "127.0.0.1"},
		{"absent", "127.0.0.1:1234", "", "", "127.0.0.1"},
		{"mapped address", "[::ffff:127.0.0.1]:1234", "::ffff:192.0.2.5", "", "192.0.2.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tc.peer
			if tc.forwarded != "" {
				req.Header.Add("X-Forwarded-For", tc.forwarded)
			}
			if tc.real != "" {
				req.Header.Add("X-Real-IP", tc.real)
			}
			if got := s.clientIP(req).String(); got != tc.want {
				t.Fatalf("client IP = %s; want %s", got, tc.want)
			}
		})
	}
	for _, invalid := range []string{"invalid", "127.0.0.1", "::/0", "0.0.0.0/0", "127.0.0.1/32,"} {
		if _, err := config.TrustedProxyCIDRs(invalid); err == nil {
			t.Errorf("accepted invalid trusted proxy configuration %q", invalid)
		}
	}
}
