package httpapi

import (
	"bytes"
	"image"
	"io"
	"net/http"
	"strings"
	"testing"

	"qushuiyin/service/internal/media"
	fixture "qushuiyin/service/testdata/media"
)

func TestWebPSavedMetadataAndPreviewMySQL(t *testing.T) {
	for _, name := range []string{"opaque", "alpha"} {
		t.Run(name, func(t *testing.T) {
			h := newDeliveryHarness(t)
			h.policy("relay_only")
			res := h.req("GET", "/v1/results/"+h.rid, "", nil, 200)
			h.aid = res["cover"].(map[string]any)["assetId"].(string)
			data := fixture.WebP(name)
			h.s.Relay.Client.Transport = durableMediaTransport(func(r *http.Request) (*http.Response, error) {
				var body io.ReadCloser = http.NoBody
				if r.Method != "HEAD" {
					h.gets++
					body = io.NopCloser(bytes.NewReader(data))
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/webp"}}, ContentLength: int64(len(data)), Body: body}, nil
			})
			// Remove preview-only rows as well as ordinary download-backed cache rows.
			t.Cleanup(func() {
				rows, e := h.db.Query("SELECT id,storage_key FROM media_file")
				if e != nil {
					return
				}
				var ids []string
				for rows.Next() {
					var id, key string
					rows.Scan(&id, &key)
					if f, e := h.s.Relay.Open(key); e == nil {
						f.Close()
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					h.db.Exec("DELETE FROM media_file WHERE id=?", id)
				}
			})
			bid, iid, item := h.batch()
			want := "image/jpeg"
			if name == "alpha" {
				want = "image/png"
			}
			if item["prepareStatus"] != "READY" || item["deliveryMode"] != "relay" || item["converted"] != true || item["mimeType"] != want {
				t.Fatal("wrong WebP batch", item)
			}
			var converted bool
			var size int64
			var hash string
			if e := h.db.QueryRow("SELECT f.converted,f.size_bytes,f.sha256 FROM media_file f JOIN media_object o ON o.file_id=f.id WHERE o.user_id=?", h.user).Scan(&converted, &size, &hash); e != nil || !converted || size <= 0 || len(hash) != 64 {
				t.Fatal("wrong stored conversion", e)
			}
			tickets := h.req("POST", "/v1/save-batches/"+bid+"/tickets", "", dto{"itemIds": []string{iid}}, 200)
			address := tickets["tickets"].([]any)[0].(map[string]any)["url"].(string)
			w := previewRequest(h, "GET", address, "", 200)
			if w.Header().Get("Content-Type") != want || int64(w.Body.Len()) != size {
				t.Fatal("download describes source")
			}
			old := h.s.Relay
			h.s.Relay = &media.Relay{DB: old.DB, Domains: old.Domains, Client: old.Client, Settings: old.Settings, Key: old.Key, Directory: old.Directory, FFprobe: old.FFprobe}
			_, _, again := h.batch()
			if again["converted"] != true || h.gets != 1 {
				t.Fatal("cache lost converted metadata", again, h.gets)
			}
			thumb := previewURL(h, h.aid)
			w = previewRequest(h, "GET", thumb, "", 200)
			_, format, e := image.Decode(bytes.NewReader(w.Body.Bytes()))
			if e != nil || format != "jpeg" {
				t.Fatal("WebP thumbnail", e)
			}
			w = previewRequest(h, "GET", strings.Replace(thumb, "/thumbnail/", "/image/", 1), "", 200)
			if w.Header().Get("Content-Type") != want || h.gets != 2 {
				t.Fatal("large preview failed to reuse converted file")
			}
		})
	}
}

func TestWebPAnimationIsNotSavedOrCopiedMySQL(t *testing.T) {
	h := newDeliveryHarness(t)
	h.policy("relay_only")
	res := h.req("GET", "/v1/results/"+h.rid, "", nil, 200)
	h.aid = res["cover"].(map[string]any)["assetId"].(string)
	enc, e := h.s.sealTask(h.aid, "candidates", []string{"https://" + h.host + "/animation.webp", "https://" + h.host + "/still.png"})
	if e != nil {
		t.Fatal(e)
	}
	h.db.Exec("UPDATE media_asset SET source_candidates_enc=? WHERE id=?", enc, h.aid)
	h.s.Relay.Client.Transport = durableMediaTransport(func(r *http.Request) (*http.Response, error) {
		data := fixture.WebP("animated")
		mime := "image/webp"
		if strings.HasSuffix(r.URL.Path, ".png") {
			mime, data = fixture.File("image")
		}
		var body io.ReadCloser = http.NoBody
		if r.Method != "HEAD" {
			h.gets++
			body = io.NopCloser(bytes.NewReader(data))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, ContentLength: int64(len(data)), Body: body}, nil
	})
	bid, iid, item := h.batch()
	if item["prepareStatus"] != "FAILED" || item["copyLinkAvailable"] != false || item["converted"] != false || item["error"].(map[string]any)["code"] != "FORMAT_UNSUPPORTED" || h.gets != 1 {
		t.Fatal("animated source was accepted", item, h.gets)
	}
	h.req("POST", "/v1/save-batches/"+bid+"/tickets", "", dto{"itemIds": []string{iid}}, 409)
}
