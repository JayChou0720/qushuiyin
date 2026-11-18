package parse

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"qushuiyin/service/internal/outbound"
)

// Only this Bilibili archive host has been verified for HTTP-to-HTTPS upgrades.
func bilibiliCoverUpgradeAllowed(u *url.URL) bool {
	return outbound.HTTPS(u) && strings.EqualFold(u.Hostname(), "i2.hdslb.com") && strings.HasPrefix(u.Path, "/bfs/archive/")
}

func (a Adapter) bilibiliCover(ctx context.Context, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if outbound.HTTPS(u) {
		return raw
	}
	if u.Scheme != "http" || u.Port() != "" || a.coverClient.Transport == nil || ctx.Err() != nil {
		return ""
	}
	u.Scheme = "https"
	resp, err := a.coverClient.Headers(ctx, u.String(), http.MethodHead, "", bilibiliCoverUpgradeAllowed)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 || resp.ContentLength > a.Limits.ImageBytes {
		return ""
	}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	switch kind {
	case "image/jpeg", "image/png", "image/webp":
		return resp.Request.URL.String()
	default:
		return ""
	}
}
