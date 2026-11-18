package parse

import (
	"net/url"
	"qushuiyin/service/internal/outbound"
	"regexp"
	"strings"
)

type Code string

func (c Code) Error() string { return string(c) }

const (
	Invalid         Code = "LINK_INVALID"
	Forbidden       Code = "LINK_NOT_ALLOWED"
	Unsupported     Code = "PLATFORM_UNSUPPORTED"
	TypeUnsupported Code = "CONTENT_TYPE_UNSUPPORTED"
	InvalidResponse Code = "UPSTREAM_INVALID_RESPONSE"
	Unavailable     Code = "UPSTREAM_UNAVAILABLE"
	Maintenance     Code = "PLATFORM_MAINTENANCE"
	Busy            Code = "SERVICE_BUSY"
	Timeout         Code = "PARSE_TIMEOUT"
	Canceled        Code = "CANCELLED"
)

var domains = map[string][]string{"douyin": {"v.douyin.com", "www.iesdouyin.com", "www.douyin.com"}, "xiaohongshu": {"www.xiaohongshu.com", "xhslink.com", "xhslink.cn"}, "kuaishou": {"v.kuaishou.com", "www.kuaishou.com"}, "bilibili": {"bilibili.com", "b23.tv"}}

func Platform(u *url.URL) string {
	if !outbound.HTTPS(u) {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	for p, list := range domains {
		for _, d := range list {
			if h == d || strings.HasSuffix(h, "."+d) {
				return p
			}
		}
	}
	return ""
}

var patterns = map[string]*regexp.Regexp{
	"douyin":      regexp.MustCompile("^/(?:share/)?(?:video|note|slides)/([0-9]+)/?$"),
	"xiaohongshu": regexp.MustCompile("^/(?:explore|discovery/item)/([a-zA-Z0-9]+)/?$"),
	"kuaishou":    regexp.MustCompile("^/(?:short-video|photo)/([a-zA-Z0-9]+)/?$"),
	"bilibili":    regexp.MustCompile("^/video/(BV[a-zA-Z0-9]+|av[0-9]+)/?$"),
}

func contentID(p string, u *url.URL) *string {
	if u == nil {
		return nil
	}
	m := patterns[p].FindStringSubmatch(u.Path)
	if len(m) == 0 {
		return nil
	}
	return &m[1]
}
func biliAllowed(u *url.URL) bool {
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, v := range q["p"] {
		if v != "1" {
			return false
		}
	}
	h := strings.ToLower(u.Hostname())
	return h == "b23.tv" || (contentID("bilibili", u) != nil && (h == "bilibili.com" || h == "www.bilibili.com" || h == "m.bilibili.com"))
}
