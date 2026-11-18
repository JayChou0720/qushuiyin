package media

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"qushuiyin/service/internal/config"
	"qushuiyin/service/internal/outbound"
)

type relayTransport func(*http.Request) (*http.Response, error)

func (f relayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRelayFetchValidatesBytesBeforePublishing(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mime string
		body       []byte
		limit      int64
		valid      bool
	}{
		{"generic PNG", "application/octet-stream", pngBytes.Bytes(), 1024, true},
		{"HTML disguised as image", "image/png", []byte("<html>denied</html>"), 1024, false},
		{"wrong declared type", "video/mp4", pngBytes.Bytes(), 1024, false},
		{"truncated PNG", "image/png", pngBytes.Bytes()[:40], 1024, false},
		{"unknown length over limit", "image/png", pngBytes.Bytes(), 8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Relay{Directory: t.TempDir(), Settings: relaySettings(tc.limit), Client: outbound.Client{Redirects: 5, Transport: relayTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.mime}}, ContentLength: -1, Body: io.NopCloser(bytes.NewReader(tc.body))}, nil
			})}}
			file, err := r.fetch(context.Background(), "https://cdn.example.test/image", "image")
			entries, e := os.ReadDir(r.Directory)
			if e != nil {
				t.Fatal(e)
			}
			if !tc.valid {
				if err == nil || len(entries) != 0 {
					t.Fatalf("invalid response published a file: %v %v", err, entries)
				}
				return
			}
			if err != nil || file.MIME != "image/png" || file.Size != int64(pngBytes.Len()) || len(file.Hash) != 64 || len(entries) != 1 || filepath.Ext(entries[0].Name()) != ".media" {
				t.Fatalf("bad prepared file: %+v %v", file, err)
			}
		})
	}
}
func relaySettings(limit int64) config.Values {
	data := map[string]json.RawMessage{}
	for k, v := range map[string]int64{"max_image_bytes": limit, "max_image_pixels": 40000000, "relay_max_video_bytes": limit, "media_fetch_timeout_seconds": 90, "max_media_candidates": 3, "media_cache_ttl_seconds": 900, "direct_probe_range_bytes": 4096, "media_cache_budget_bytes": 5368709120} {
		b, _ := json.Marshal(v)
		data[k] = b
	}
	return config.Values{Data: data}
}
