package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const placeholderPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>MockVision</title></head>
<body><h1>MockVision</h1><p>The panel is not built into this binary. Run <code>make frontend</code> and rebuild,
or use the API under <code>/api/v1</code>.</p></body></html>`

// noncePlaceholder is where index.html takes the nonce of its response.
const noncePlaceholder = "__MOCKVISION_CSP_NONCE__"

// spaHandler serves the embedded panel: files as they are, hashed assets
// cached forever, and index.html for client-side routes. index.html gets a
// nonce of its own on every response: the style elements the panel's
// components add at run time carry it (a modal dialog locks the page's
// scroll with one), and no other inline style is allowed (D49).
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
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "panel unavailable", http.StatusInternalServerError)
			return
		}
		nonce := newNonce()
		// A page and its nonce are never cached.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", cspWithStyleNonce(nonce))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(bytes.ReplaceAll(index, []byte(noncePlaceholder), []byte(nonce)))
	})
}

func newNonce() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
