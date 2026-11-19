package admin

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var bundle embed.FS

func SPA() http.Handler {
	dist, err := fs.Sub(bundle, "dist")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/admin/", http.FileServerFS(dist))
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		path := strings.TrimPrefix(r.URL.Path, "/admin/")
		switch {
		case path == "v1" || strings.HasPrefix(path, "v1/"):
			http.NotFound(w, r)
		case strings.HasPrefix(path, "assets/") && !strings.HasSuffix(path, "/"):
			if info, err := fs.Stat(dist, path); err == nil && !info.IsDir() {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				files.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		case strings.Contains(path, "."):
			http.NotFound(w, r)
		default:
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.Method != http.MethodHead {
				_, _ = w.Write(index)
			}
		}
	})
}
