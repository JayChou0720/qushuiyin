// Package media contains deliberately synthetic fixtures used by local mock mode.
package media

import _ "embed"

//go:embed sample.mp4
var video []byte

//go:embed cover.png
var cover []byte

func File(kind string) (string, []byte) {
	if kind == "video" {
		return "video/mp4", video
	}
	return "image/png", cover
}
