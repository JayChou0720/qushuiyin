package media

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

func (r *Relay) imageWork(ctx context.Context) (func(), error) {
	r.mu.Lock()
	if r.imageSlots == nil {
		n := r.Settings.Int("image_convert_workers")
		if n <= 0 {
			r.mu.Unlock()
			return nil, errors.New("SERVICE_BUSY")
		}
		r.imageSlots = make(chan struct{}, int(n))
	}
	slots := r.imageSlots
	r.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, errors.New("SERVICE_BUSY")
	}
}

func (r *Relay) processImage(ctx context.Context, f *os.File, mime string, thumbnail bool) (string, error) {
	if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
		return "", errors.New("FORMAT_UNSUPPORTED")
	}
	release, err := r.imageWork(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	alpha := false
	if mime == "image/webp" {
		alpha, err = inspectWebP(ctx, f, r.Settings.Int("max_image_pixels"))
		if err != nil {
			return "", err
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "", errors.New("FORMAT_UNSUPPORTED")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > r.Settings.Int("max_image_pixels")/int64(cfg.Height) {
		return "", errors.New("FORMAT_UNSUPPORTED")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return "", errors.New("FORMAT_UNSUPPORTED")
	}
	if img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
		return "", errors.New("FORMAT_UNSUPPORTED")
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if !thumbnail && mime != "image/webp" {
		return mime, nil
	}
	quality := r.Settings.Int("thumbnail_jpeg_quality")
	if quality < 1 || quality > 100 {
		return "", errors.New("SERVICE_BUSY")
	}
	if thumbnail {
		edge := r.Settings.Int("thumbnail_max_edge_px")
		if edge <= 0 {
			return "", errors.New("SERVICE_BUSY")
		}
		w, h := cfg.Width, cfg.Height
		if int64(max(w, h)) > edge {
			if w >= h {
				h = max(1, int(int64(h)*edge/int64(w)))
				w = int(edge)
			} else {
				w = max(1, int(int64(w)*edge/int64(h)))
				h = int(edge)
			}
		}
		thumb := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(thumb, thumb.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.CatmullRom.Scale(thumb, thumb.Bounds(), img, img.Bounds(), draw.Over, nil)
		img = thumb
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = f.Truncate(0); err != nil {
		return "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	w := &boundedImageWriter{w: f, left: r.Settings.Int("max_image_bytes"), ctx: ctx}
	if alpha && !thumbnail {
		return "image/png", png.Encode(w, img)
	}
	err = jpeg.Encode(w, img, &jpeg.Options{Quality: int(quality)})
	return "image/jpeg", err
}

type boundedImageWriter struct {
	w    io.Writer
	left int64
	ctx  context.Context
}

func (w *boundedImageWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.left {
		return 0, errors.New("FILE_TOO_LARGE")
	}
	n, e := w.w.Write(p)
	w.left -= int64(n)
	return n, e
}
