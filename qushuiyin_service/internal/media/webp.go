package media

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
)

var errAnimatedWebP = errors.New("FORMAT_UNSUPPORTED")

// Inspect the complete container before decoding. A decoder may stop after the
// first frame/chunk; that must not silently turn an animation into a still.
// Check both canvas and encoded dimensions before either can allocate pixels.
func inspectWebP(ctx context.Context, f *os.File, maxPixels int64) (bool, error) {
	bad := errors.New("FORMAT_UNSUPPORTED")
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	var head [12]byte
	if _, err = f.ReadAt(head[:], 0); err != nil || string(head[:4]) != "RIFF" || string(head[8:]) != "WEBP" || int64(binary.LittleEndian.Uint32(head[4:8]))+8 != info.Size() {
		return false, bad
	}
	validPixels := func(w, h int64) bool { return w > 0 && h > 0 && maxPixels > 0 && w <= maxPixels/h }
	alpha := false
	var canvasW, canvasH int64
	codecs := 0
	for pos := int64(12); pos < info.Size(); {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		var chunk [8]byte
		if _, err = f.ReadAt(chunk[:], pos); err != nil {
			return false, bad
		}
		n := int64(binary.LittleEndian.Uint32(chunk[4:]))
		start := pos + 8
		end := start + n
		if end > info.Size() || end+(n&1) > info.Size() {
			return false, bad
		}
		var buf [10]byte
		var w, h int64
		switch string(chunk[:4]) {
		case "ANIM", "ANMF":
			return false, errAnimatedWebP
		case "VP8X":
			if pos != 12 || n != 10 {
				return false, bad
			}
			if _, err = f.ReadAt(buf[:], start); err != nil {
				return false, bad
			}
			if buf[0]&2 != 0 {
				return false, errAnimatedWebP
			}
			if buf[0]&0xc1 != 0 || buf[1] != 0 || buf[2] != 0 || buf[3] != 0 {
				return false, bad
			}
			alpha = buf[0]&0x10 != 0
			canvasW = 1 + int64(buf[4]) + (int64(buf[5]) << 8) + (int64(buf[6]) << 16)
			canvasH = 1 + int64(buf[7]) + (int64(buf[8]) << 8) + (int64(buf[9]) << 16)
			if !validPixels(canvasW, canvasH) {
				return false, bad
			}
		case "ALPH":
			alpha = true
		case "VP8L":
			if n < 5 {
				return false, bad
			}
			if _, err = f.ReadAt(buf[:5], start); err != nil || buf[0] != 0x2f {
				return false, bad
			}
			bits := binary.LittleEndian.Uint32(buf[1:5])
			if bits>>29 != 0 {
				return false, bad
			}
			w = int64(bits&0x3fff) + 1
			h = int64((bits>>14)&0x3fff) + 1
			alpha = alpha || bits&(1<<28) != 0
		case "VP8 ":
			if n < 10 {
				return false, bad
			}
			if _, err = f.ReadAt(buf[:], start); err != nil || string(buf[3:6]) != "\x9d\x01\x2a" {
				return false, bad
			}
			w = int64(binary.LittleEndian.Uint16(buf[6:8]) & 0x3fff)
			h = int64(binary.LittleEndian.Uint16(buf[8:10]) & 0x3fff)
		}
		if w != 0 || h != 0 {
			codecs++
			if codecs != 1 || !validPixels(w, h) || canvasW != 0 && (w != canvasW || h != canvasH) {
				return false, bad
			}
		}
		pos = end + (n & 1)
	}
	if codecs != 1 {
		return false, bad
	}
	return alpha, nil
}
