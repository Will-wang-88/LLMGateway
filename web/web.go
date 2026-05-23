// Package web serves the embedded HTML/CSS/JS dashboard. The dashboard talks
// to the /admin/* API; auth is performed there.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var assets embed.FS

// Register installs the dashboard handler on the given mux. The dashboard is
// rooted at /ui/. The root path / is redirected to /ui/.
func Register(mux *http.ServeMux) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	mux.Handle("GET /ui/", &staticHandler{fs: sub})
}

type staticHandler struct{ fs fs.FS }

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/ui/")
	if p == "" || !strings.Contains(p, ".") {
		p = "index.html"
	}
	p = path.Clean("/" + p)[1:] // normalise, drop leading slash for fs.FS
	if p == "" {
		p = "index.html"
	}
	data, err := fs.ReadFile(h.fs, p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(data))
}
