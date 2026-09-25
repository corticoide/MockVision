package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const placeholderPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>MockVision</title></head>
<body><h1>MockVision</h1><p>The panel is not built into this binary. Run <code>make frontend</code> and rebuild,
or use the API under <code>/api/v1</code>.</p></body></html>`

// spaHandler serves the embedded panel: files as they are, hashed assets
// cached forever, and index.html for client-side routes.
func spaHandler(dist fs.FS) http.Handler {
	_, err := fs.Stat(dist, "index.html")
	built := err == nil
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(placeholderPage))
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "index.html" {
			if st, err := fs.Stat(dist, p); err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "panel unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
}
