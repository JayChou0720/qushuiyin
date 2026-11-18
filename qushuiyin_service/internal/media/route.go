package media

import (
	"context"
	"strings"
	"time"

	"qushuiyin/service/internal/parse"
)

// Decision is internal. Only the ticket/copy endpoints may disclose URL.
type Decision struct {
	Mode, URL, Host, MIME, Failure string
	Size                           *int64
	Expires                        time.Time
	CopyAvailable, Unregistered    bool
}

func (r *Relay) Decide(ctx context.Context, platform, policy string, a parse.Asset, fallback bool) Decision {
	if policy == "relay_only" && r.Domains == nil {
		return Decision{Mode: "relay"}
	}
	l := r.Settings.Limits()
	o := (Probe{Client: r.Client, Limits: l, Domains: r.Domains, Policy: policy}).Inspect(ctx, platform, a)
	d := Decision{URL: o.FinalURL, Host: o.Host, MIME: o.Type, Size: o.Size, Expires: time.Now().UTC().Add(l.AssumedTTL), Unregistered: o.Host != "" && o.Domain != "registered"}
	validType := o.Type == "image/jpeg" || o.Type == "image/png" || o.Type == "image/webp"
	if a.Kind == "video" {
		validType = strings.HasPrefix(o.Type, "video/")
	}
	if !validType {
		d.URL = ""
	}
	if o.Reason == "DOMAIN_STORE_FAILED" {
		d.Failure = "SERVICE_BUSY"
		d.URL = ""
		return d
	}
	if policy == "direct_preferred" && !fallback && o.Direct {
		d.Mode = "direct"
		return d
	}
	limit := r.Settings.Int("max_image_bytes")
	if a.Kind == "video" {
		limit = r.Settings.Int("relay_max_video_bytes")
	}
	if policy != "copy_only" && (o.Size == nil || *o.Size <= limit) {
		d.Mode = "relay"
		return d
	}
	d.Failure = "DIRECT_DOWNLOAD_UNAVAILABLE"
	maximum := r.Settings.Int("max_image_bytes")
	if a.Kind == "video" {
		maximum = r.Settings.Int("max_video_bytes")
	}
	if o.Size != nil && *o.Size > maximum {
		d.Failure = "FILE_TOO_LARGE"
	}
	d.CopyAvailable = d.URL != "" && policy != "relay_only"
	return d
}
