package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"qushuiyin/service/internal/outbound"
	fixture "qushuiyin/service/testdata/media"
)

func webpRelay(t *testing.T, body []byte) *Relay {
	t.Helper()
	return &Relay{Directory: t.TempDir(), Settings: relaySettings(1 << 20), Client: outbound.Client{Transport: relayTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}}
}

func TestWebPConversionPreservesDimensionsAndTransparency(t *testing.T) {
	for _, name := range []string{"opaque", "opaque-lossless", "alpha", "alpha-lossless"} {
		t.Run(name, func(t *testing.T) {
			r := webpRelay(t, fixture.WebP(name))
			p, e := r.fetch(t.Context(), "https://cdn.example.test/image", "image")
			if e != nil {
				t.Fatal(e)
			}
			f, e := r.Open(p.StorageKey)
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			out, e := io.ReadAll(f)
			if e != nil {
				t.Fatal(e)
			}
			img, format, e := image.Decode(bytes.NewReader(out))
			if e != nil {
				t.Fatal(e)
			}
			want := "jpeg"
			if name == "alpha" || name == "alpha-lossless" {
				want = "png"
				_, _, _, a := img.At(0, 0).RGBA()
				if a >= 65535 || a == 0 {
					t.Fatal("alpha lost", a)
				}
			}
			if format != want || p.MIME != "image/"+want || !p.Converted || img.Bounds().Dx() != 32 || img.Bounds().Dy() != 24 {
				t.Fatal("wrong conversion", format, p)
			}
			digest := sha256.Sum256(out)
			if p.Hash != hex.EncodeToString(digest[:]) || p.Size != int64(len(out)) || bytes.Equal(out, fixture.WebP(name)) {
				t.Fatal("output metadata describes source")
			}
			thumb, e := r.fetchOutput(t.Context(), "https://cdn.example.test/image", "image", true)
			if e != nil || thumb.MIME != "image/jpeg" {
				t.Fatal("WebP thumbnail unavailable", e)
			}
		})
	}
}

func TestWebPRejectsAnimationsMalformedAndExcessPixels(t *testing.T) {
	trailing := append([]byte(nil), fixture.WebP("opaque")...)
	trailing = append(trailing, []byte{'A', 'N', 'I', 'M', 0, 0, 0, 0}...)
	binary.LittleEndian.PutUint32(trailing[4:8], uint32(len(trailing)-8))
	badLength := append([]byte(nil), fixture.WebP("opaque")...)
	binary.LittleEndian.PutUint32(badLength[4:8], 0xffffffff)
	for _, tc := range []struct {
		name   string
		body   []byte
		pixels int64
	}{
		{"animation", fixture.WebP("animated"), 40000000},
		{"animation after still", trailing, 40000000},
		{"truncated", fixture.WebP("alpha")[:30], 40000000},
		{"invalid RIFF length", badLength, 40000000},
		{"lossy pixel limit", fixture.WebP("opaque"), 100},
		{"lossless pixel limit", fixture.WebP("opaque-lossless"), 100},
		{"canvas pixel limit", fixture.WebP("alpha"), 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := webpRelay(t, tc.body)
			r.Settings.Data["max_image_pixels"], _ = json.Marshal(tc.pixels)
			_, e := r.fetch(t.Context(), "https://cdn.example.test/image", "image")
			if e == nil || e.Error() != "FORMAT_UNSUPPORTED" {
				t.Fatal("invalid WebP accepted", e)
			}
			entries, e := os.ReadDir(r.Directory)
			if e != nil || len(entries) != 0 {
				t.Fatal("rejected file retained", entries, e)
			}
		})
	}
}

func TestWebPOutputAndWorkerBudgets(t *testing.T) {
	r := webpRelay(t, fixture.WebP("opaque"))
	r.Settings.Data["max_image_bytes"], _ = json.Marshal(len(fixture.WebP("opaque")) + 1)
	if _, e := r.fetch(t.Context(), "https://cdn.example.test/image", "image"); e == nil || e.Error() != "FILE_TOO_LARGE" {
		t.Fatal("unbounded conversion output", e)
	}
	r.Settings.Data["max_image_bytes"] = json.RawMessage(`1048576`)
	release, e := r.imageWork(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, e = r.fetch(ctx, "https://cdn.example.test/image", "image")
	release()
	if e == nil || e.Error() != "SERVICE_BUSY" {
		t.Fatal("conversion bypassed workers", e)
	}
	entries, e := os.ReadDir(r.Directory)
	if e != nil || len(entries) != 0 {
		t.Fatal("budget failure retained files", entries, e)
	}
}
