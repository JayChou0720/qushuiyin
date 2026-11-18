// Package media contains deliberately synthetic fixtures used by local mock mode.
package media

import _ "embed"

//go:embed sample.mp4
var video []byte

//go:embed cover.png
var cover []byte

// These tiny WebP images were generated from synthetic pixels with FFmpeg.
//
//go:embed opaque.webp
var opaqueWebP []byte

//go:embed opaque-lossless.webp
var opaqueLosslessWebP []byte

//go:embed alpha.webp
var alphaWebP []byte

//go:embed alpha-lossless.webp
var alphaLosslessWebP []byte

//go:embed animated.webp
var animatedWebP []byte

func WebP(name string) []byte {
	switch name {
	case "opaque":
		return opaqueWebP
	case "opaque-lossless":
		return opaqueLosslessWebP
	case "alpha":
		return alphaWebP
	case "alpha-lossless":
		return alphaLosslessWebP
	case "animated":
		return animatedWebP
	}
	return nil
}

func File(kind string) (string, []byte) {
	if kind == "video" {
		return "video/mp4", video
	}
	return "image/png", cover
}
