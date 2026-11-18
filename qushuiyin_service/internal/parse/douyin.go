package parse

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/wujunwei928/parse-video/parser"
	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
	"qushuiyin/service/internal/outbound"
)

const shareOrigin = "https://www.iesdouyin.com"
const itemPath = "/web/api/v2/aweme/iteminfo/"
const mobileUA = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 Chrome/120.0 Mobile Safari/537.36"
const DouyinH5Version = "douyin-h5-iteminfo-v1"

// Same maximum response size as the pinned parser (§12.1).
const h5MaxBytes = 16 << 20

func h5Host(u *url.URL) bool {
	if !outbound.HTTPS(u) {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, d := range []string{"douyin.com", "iesdouyin.com", "snssdk.com"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

type shareSession struct{ token, webID, cip string }

func readSharePage(data []byte) shareSession {
	var s shareSession
	z := html.NewTokenizer(bytes.NewReader(data))
	for {
		typ := z.Next()
		if typ == html.ErrorToken {
			break
		}
		if typ != html.StartTagToken && typ != html.SelfClosingTagToken {
			continue
		}
		tag := z.Token()
		var id, token, web, cip string
		for _, a := range tag.Attr {
			switch a.Key {
			case "id":
				id = a.Val
			case "xsstoken":
				token = a.Val
			case "webid":
				web = a.Val
			case "usercip":
				cip = a.Val
			}
		}
		switch id {
		case "douyin_reflow_token":
			s.token = token
		case "douyin_reflow_webId":
			s.webID = web
			s.cip = cip
		}
		if s.token != "" && s.webID != "" {
			break
		}
	}
	return s
}
func (s shareSession) params(work string) (url.Values, error) {
	if s.token == "" || len(s.webID) < aes.BlockSize {
		return nil, Unavailable
	}
	key := []byte(s.webID)[:aes.BlockSize]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, Unavailable
	}
	data := []byte(s.token)
	pad := aes.BlockSize - len(data)%aes.BlockSize
	data = append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
	cipher.NewCBCEncrypter(block, key).CryptBlocks(data, data)
	q := url.Values{"reflow_source": {"reflow_page"}, "web_id": {s.webID}, "device_id": {s.webID}, "use_new_select_scope": {"0"}, "item_ids": {work}, "reflow_id": {base64.StdEncoding.EncodeToString(data)}}
	if s.cip != "" {
		q.Set("user_cip", s.cip)
	}
	return q, nil
}

// H5 requests share only explicitly recognized anonymous cookies on the original host.
// A fresh jar per parse keeps even simultaneous requests independent.
func shareGet(ctx context.Context, c outbound.Client, jar http.CookieJar, raw, referer string) ([]byte, error) {
	u, err := url.Parse(raw)
	seen := map[string]bool{}
	for hops := 0; err == nil && hops <= c.Redirects; hops++ {
		if !h5Host(u) || seen[u.String()] {
			return nil, Unavailable
		}
		if referer != "" && (u.Hostname() != "www.iesdouyin.com" || u.Path != itemPath) {
			return nil, Unavailable
		}
		seen[u.String()] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, Unavailable
		}
		req.Header.Set("User-Agent", mobileUA)
		if u.Hostname() == "www.iesdouyin.com" {
			if referer != "" {
				req.Header.Set("Referer", referer)
			}
			for _, cookie := range jar.Cookies(u) {
				req.AddCookie(cookie)
			}
		}
		resp, err := c.Transport.RoundTrip(req)
		if err != nil {
			return nil, Unavailable
		}
		resp.Request = req
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			u, err = resp.Location()
			resp.Body.Close()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, Unavailable
		}
		if referer == "" && u.Hostname() == "www.iesdouyin.com" {
			for _, cookie := range resp.Cookies() {
				switch cookie.Name {
				case "ttwid", "__ac_nonce", "tt_webid", "s_v_web_id":
					jar.SetCookies(u, []*http.Cookie{cookie})
				}
			}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, h5MaxBytes+1))
		resp.Body.Close()
		if err != nil || len(body) > h5MaxBytes {
			return nil, Unavailable
		}
		return body, nil
	}
	return nil, Unavailable
}
func douyinItem(ctx context.Context, c outbound.Client, work string) (*parser.VideoParseInfo, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, Unavailable
	}
	page := shareOrigin + "/share/video/" + work + "/"
	body, err := shareGet(ctx, c, jar, page, "")
	if err != nil {
		return nil, err
	}
	q, err := readSharePage(body).params(work)
	if err != nil {
		return nil, err
	}
	body, err = shareGet(ctx, c, jar, shareOrigin+itemPath+"?"+q.Encode(), page)
	if err != nil {
		return nil, err
	}
	return parseItem(body, work)
}

type h5URLs struct {
	URLs []string `json:"url_list"`
}

func (u h5URLs) first() string {
	if len(u.URLs) == 0 {
		return ""
	}
	return u.URLs[0]
}
func parseItem(data []byte, work string) (*parser.VideoParseInfo, error) {
	var reply struct {
		Status *int `json:"status_code"`
		Items  []struct {
			ID    string `json:"aweme_id"`
			Desc  string
			Video struct {
				Play  h5URLs `json:"play_addr"`
				Cover h5URLs
			}
			Images []h5URLs
		} `json:"item_list"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Status == nil {
		return nil, InvalidResponse
	}
	if *reply.Status != 0 {
		return nil, Unavailable
	}
	if len(reply.Items) == 0 {
		return nil, Unavailable
	}
	item := reply.Items[0]
	if item.ID != work {
		return nil, InvalidResponse
	}
	v := item.Video.Play.first()
	if u, err := url.Parse(v); err == nil {
		original := u.EscapedPath()
		path := strings.Replace(original, "/playwm/", "/play/", 1)
		if base, ok := strings.CutSuffix(path, "/playwm"); ok {
			path = base + "/play"
		}
		if path != original {
			u.Path, _ = url.PathUnescape(path)
			u.RawPath = path
			v = u.String()
		}
	}
	info := &parser.VideoParseInfo{Title: item.Desc, VideoUrl: v, CoverUrl: item.Video.Cover.first()}
	for _, image := range item.Images {
		info.Images = append(info.Images, parser.ImgInfo{Url: image.first()})
	}
	return info, nil
}
