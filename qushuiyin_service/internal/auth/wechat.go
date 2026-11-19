package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

var ErrWeChatCode = errors.New("WeChat rejected the login code")
var errWeChatExchange = errors.New("WeChat login exchange unavailable")

type WeChat struct {
	AppID, AppSecret string
	Client           *http.Client
}

func NewWeChat(appID, appSecret string, timeout time.Duration) WeChat {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return WeChat{AppID: appID, AppSecret: appSecret, Client: &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (c WeChat) OpenID(ctx context.Context, code string) (string, error) {
	query := url.Values{"appid": {c.AppID}, "secret": {c.AppSecret}, "js_code": {code}, "grant_type": {"authorization_code"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.weixin.qq.com/sns/jscode2session?"+query.Encode(), nil)
	if err != nil {
		return "", errWeChatExchange
	}
	res, err := c.Client.Do(req)
	if err != nil {
		// The transport error can include the request URL and its credentials.
		return "", errWeChatExchange
	}
	defer res.Body.Close()
	var payload struct {
		ErrCode int    `json:"errcode"`
		OpenID  string `json:"openid"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&payload) != nil {
		return "", errWeChatExchange
	}
	if payload.ErrCode == 40029 || payload.ErrCode == 40163 {
		return "", ErrWeChatCode
	}
	if payload.ErrCode != 0 || payload.OpenID == "" || len(payload.OpenID) > 128 {
		return "", errWeChatExchange
	}
	return payload.OpenID, nil
}
