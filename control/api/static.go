package api

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// staticUI serves the built single-page app from LA_UI_DIR. Lookups go
// through os.Root, so neither ".." nor a symlink can leave the directory.
type staticUI struct {
	root *os.Root
}

func newStaticUI(dir string) (*staticUI, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("api: LA_UI_DIR: %w", err)
	}
	return &staticUI{root: root}, nil
}

func (u *staticUI) Close() { _ = u.root.Close() }

// Content types by extension, so that serving does not depend on the host's
// mime database (the container image has none).
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8", ".json": "application/json", ".map": "application/json",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".ico": "image/x-icon", ".woff": "font/woff", ".woff2": "font/woff2",
	".txt": "text/plain; charset=utf-8", ".wasm": "application/wasm", ".webmanifest": "application/manifest+json",
}

const (
	// Files under /assets/ carry a content hash in their name.
	cacheImmutable = "public, max-age=31536000, immutable"
	// Everything else (index.html above all) is revalidated on every load,
	// so a deploy is picked up immediately.
	cacheRevalidate = "no-cache"
)

func (u *staticUI) open(rel string) (*os.File, os.FileInfo, bool) {
	f, err := u.root.Open(rel)
	if err != nil {
		return nil, nil, false
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, false
	}
	return f, fi, true
}

func (u *staticUI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, errCode(http.StatusMethodNotAllowed, "method_not_allowed", "Bu yöntem desteklenmiyor"))
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(clean, "/")
	hidden := false
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			hidden = true
		}
	}
	isAsset := strings.HasPrefix(clean, "/assets/")
	if rel != "" && rel != "index.html" && !hidden {
		if f, fi, ok := u.open(rel); ok {
			defer f.Close()
			cache := cacheRevalidate
			if isAsset {
				cache = cacheImmutable
			}
			u.send(w, r, f, fi, path.Ext(rel), cache)
			return
		}
	}
	if isAsset || hidden {
		// A missing hashed asset is a real 404 (answering it with HTML
		// would only produce a confusing script error); dotfiles are never
		// served.
		writeError(w, errNotFound())
		return
	}
	// Unknown paths are client-side routes: hand out the SPA shell.
	f, fi, ok := u.open("index.html")
	if !ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "UI is not built: LA_UI_DIR has no index.html\n")
		return
	}
	defer f.Close()
	u.send(w, r, f, fi, ".html", cacheRevalidate)
}

func (u *staticUI) send(w http.ResponseWriter, r *http.Request, f *os.File, fi os.FileInfo, ext, cache string) {
	ct := contentTypes[strings.ToLower(ext)]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cache)
	// ServeContent adds Last-Modified and handles conditional and range requests.
	http.ServeContent(w, r, "", fi.ModTime(), f)
}
