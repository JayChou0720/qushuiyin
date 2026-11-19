package auth

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type testTransport func(*http.Request) (*http.Response, error)

func (fn testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestWeChatExchange(t *testing.T) {
	client := NewWeChat("wx_test", "synthetic-secret", time.Second)
	for _, tc := range []struct {
		body, openID string
		want         error
	}{
		{`{"openid":"fixture-id","session_key":"private-key"}`, "fixture-id", nil},
		{`{"errcode":40029,"errmsg":"private diagnostic"}`, "", ErrWeChatCode},
		{`{"errcode":-1}`, "", errWeChatExchange},
		{`{"session_key":"no-open-id"}`, "", errWeChatExchange},
	} {
		client.Client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
			query := r.URL.Query()
			if r.URL.Scheme != "https" || r.URL.Host != "api.weixin.qq.com" || r.URL.Path != "/sns/jscode2session" ||
				query.Get("appid") != "wx_test" || query.Get("secret") != "synthetic-secret" ||
				query.Get("js_code") != "code+with/slash" || query.Get("grant_type") != "authorization_code" {
				t.Fatal("unexpected code exchange request")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})
		openID, err := client.OpenID(t.Context(), "code+with/slash")
		if openID != tc.openID || !errors.Is(err, tc.want) {
			t.Fatalf("unexpected code exchange result: %q %v", openID, err)
		}
	}
}
