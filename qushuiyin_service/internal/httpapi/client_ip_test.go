package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/config"
)

func TestTrustedProxySeparatesLoginRateBuckets(t *testing.T) {
	proxies, err := config.TrustedProxyCIDRs("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	wechat := auth.WeChat{AppID: "wx_fixture", Client: &http.Client{Transport: durableMediaTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"errcode":40029}`))}, nil
	})}}
	s := Mock{TrustedProxies: proxies, WeChat: &wechat, rates: map[string]meter{}, Settings: config.Values{Data: map[string]json.RawMessage{
		"max_request_body_bytes":             json.RawMessage("65536"),
		"memory_rate_limit_max_subjects":     json.RawMessage("20"),
		"ip_login_requests_per_minute":       json.RawMessage("1"),
		"app_login_requests_per_minute":      json.RawMessage("10"),
		"ip_admin_login_requests_per_minute": json.RawMessage("1"),
	}}}
	for _, tc := range []struct {
		name, body string
		handler    http.HandlerFunc
	}{
		{"admin", `{"username":"!","password":"invalid"}`, s.adminLogin},
		{"app", `{"code":"fixture","privacyVersion":"` + privacyVersion + `"}`, s.login},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login := func(peer, forwarded string, want int) {
				t.Helper()
				req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
				req.RemoteAddr = peer
				req.Header.Set("X-Forwarded-For", forwarded)
				w := httptest.NewRecorder()
				tc.handler(w, req)
				if w.Code != want || want == 429 && w.Header().Get("Retry-After") == "" {
					t.Fatalf("login returned %d; wanted %d", w.Code, want)
				}
			}
			login("127.0.0.1:17000", "192.0.2.1", 401)
			login("127.0.0.1:17000", "192.0.2.2", 401)
			login("127.0.0.1:17000", "192.0.2.1", 429)
			login("192.0.2.3:17000", "192.0.2.4", 401)
			login("192.0.2.3:17000", "192.0.2.5", 429)
		})
	}
}

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
