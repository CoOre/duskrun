package web

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler returns an http.Handler that serves the embedded SPA from dist/.
// Requests that don't map to an existing embedded file fall back to index.html
// so client-side routing works on deep links (e.g. GET /tasks). It is mounted
// last in the API router, so it never sees /api/* or /healthz.
//
// When dist/ carries no index.html — a binary built without the frontend step —
// every page serves Placeholder instead, which says so in words.
func Handler() http.Handler {
	sub, err := fs.Sub(Dist, "dist")
	if err != nil {
		panic("web: embedded dist missing: " + err.Error())
	}
	return handlerFor(sub)
}

// handlerFor builds the handler over an arbitrary FS so both modes — a real
// bundle and an unbuilt one — can be tested without rebuilding the binary.
func handlerFor(fsys fs.FS) http.Handler {
	h := &spaHandler{fsys: fsys, files: http.FileServer(http.FS(fsys))}
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		h.placeholder = true
	}
	return h
}

type spaHandler struct {
	fsys  fs.FS
	files http.Handler
	// placeholder is set when no real SPA was embedded.
	placeholder bool
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

	// A missing asset must 404 rather than fall back to index.html.
	//
	// assets/ holds the fingerprinted JS and CSS the page loads. Answering a
	// missing one with HTML — status 200, Content-Type text/html — is worse
	// than answering nothing: the browser refuses it on MIME type and renders a
	// blank page, the server logs a success, and there is no failure recorded
	// anywhere for the operator to find. A 404 names the problem.
	if strings.HasPrefix(upath, "assets/") {
		if h.placeholder || !h.exists(upath) {
			http.NotFound(w, r)
			return
		}
		// Vite fingerprints these filenames, so they are safe to cache hard.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		h.files.ServeHTTP(w, r)
		return
	}

	if upath == "" || !h.exists(upath) {
		// Root, or an unknown path → SPA fallback (client-side routing).
		h.serveIndex(w)
		return
	}
	h.files.ServeHTTP(w, r)
}

// exists reports whether upath names a regular file in the embedded FS.
func (h *spaHandler) exists(upath string) bool {
	st, err := fs.Stat(h.fsys, upath)
	return err == nil && !st.IsDir()
}

// serveIndex writes dist/index.html with a 200 and an HTML content type. It
// bypasses http.FileServer to avoid its /index.html→/ redirect on deep links.
func (h *spaHandler) serveIndex(w http.ResponseWriter) {
	if h.placeholder {
		writeHTML(w, http.StatusOK, Placeholder)
		return
	}
	index, err := h.fsys.Open("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	defer index.Close()
	data, err := io.ReadAll(index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeHTML(w, http.StatusOK, data)
}

func writeHTML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
