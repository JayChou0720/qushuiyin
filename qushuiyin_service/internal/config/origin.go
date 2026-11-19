package config

import (
	"errors"
	"net/url"
	"strings"
)

func HTTPSOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Host != strings.ToLower(u.Host) ||
		u.User != nil || u.Port() != "" && u.Port() != "443" || u.Path != "" || u.ForceQuery || u.RawQuery != "" || strings.Contains(raw, "#") {
		return nil, errors.New("expected an HTTPS origin without a path, query or fragment")
	}
	return u, nil
}
