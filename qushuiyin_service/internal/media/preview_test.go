package media

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"testing"
	"time"

	"qushuiyin/service/internal/outbound"
	"qushuiyin/service/internal/parse"
)

func TestThumbnailPixelAndWorkerLimits(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 960, 240))
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	r := &Relay{Directory: t.TempDir(), Settings: relaySettings(1 << 20), Client: outbound.Client{Transport: relayTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(data.Len()), Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(bytes.NewReader(data.Bytes()))}, nil
	})}}
	p, e := r.fetchOutput(t.Context(), "https://cdn.example.test/image", "image", true)
	if e != nil {
		t.Fatal(e)
	}
	f, e := r.Open(p.StorageKey)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	cfg, format, e := image.DecodeConfig(f)
	if e != nil || format != "jpeg" || cfg.Width != 480 || cfg.Height != 120 {
		t.Fatal(cfg, format, e)
	}
	r.Settings.Data["max_image_pixels"] = json.RawMessage(`100`)
	if _, e = r.fetchOutput(t.Context(), "https://cdn.example.test/image", "image", true); e == nil {
		t.Fatal("pixel limit bypass")
	}
	release, e := r.imageWork(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, e = r.imageWork(ctx); e == nil || e.Error() != "SERVICE_BUSY" {
		t.Fatal("unbounded image workers", e)
	}
}

func TestVideoPreviewRejectsIgnoredAndInconsistentRanges(t *testing.T) {
	for _, tc := range []struct {
		rangeValue, contentRange string
		status                   int
		valid                    bool
	}{
		{"bytes=0-11", "", 200, true},
		{"bytes=12-23", "", 200, false},
		{"bytes=12-23", "bytes 12-23/100", 206, true},
		{"bytes=12-23", "bytes 0-11/100", 206, false},
		{"bytes=12-23", "bytes 12-23/101", 206, false},
		{"bytes=0-1,4-5", "", 200, false},
	} {
		t.Run(tc.rangeValue+tc.contentRange, func(t *testing.T) {
			r := &Relay{Settings: relaySettings(1000), Client: outbound.Client{Transport: relayTransport(func(q *http.Request) (*http.Response, error) {
				if q.Method == "HEAD" {
					return &http.Response{StatusCode: 200, ContentLength: 100, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: http.NoBody}, nil
				}
				body := make([]byte, 100)
				copy(body[4:], "ftyp")
				n := 100
				if tc.status == 206 {
					n = 12
				}
				return &http.Response{StatusCode: tc.status, ContentLength: int64(n), Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Range": {tc.contentRange}}, Body: io.NopCloser(bytes.NewReader(body[:n]))}, nil
			})}}
			out, e := r.VideoPreview(t.Context(), "douyin", parse.Asset{Kind: "video", Source: "https://cdn.example.test/video"}, tc.rangeValue, false, 24)
			if (e == nil) != tc.valid {
				t.Fatal("range validation", e)
			}
			if e == nil {
				defer out.Body.Close()
				body, e := io.ReadAll(out.Body)
				if e != nil || len(body) != 12 {
					t.Fatal("unbounded body", len(body), e)
				}
			}
		})
	}
}
