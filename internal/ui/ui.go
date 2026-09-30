// Package ui serves the operator console (ADR-028): plain HTML, CSS and ES
// modules embedded in controlplane-api at /ui/, and the Agent Studio page
// built from the same modules at /studio/ (ADR-028 Rev 1.2). Both call the
// /v1 API with the user's own key and hold no authority: every rule is
// enforced by the API and PostgreSQL. Only the files under static/ are
// served, with a strict Content-Security-Policy.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var static embed.FS

// Files is the embedded console, rooted at static/.
var Files = func() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// CSP allows only the console's own scripts, styles and API calls, and
// requires Trusted Types so no string can reach an HTML sink.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

func secure(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// pages are the two pages served from the same files (ADR-028 Rev 1.2): the
// operator console and the Agent Studio page. Each serves its own HTML only.
var pages = map[string]string{"/ui/": "index.html", "/studio/": "studio.html"}

// Register mounts the console at /ui/ and the Studio page at /studio/, and
// redirects /ui and /studio there.
func Register(mux *http.ServeMux) {
	for prefix := range pages {
		mux.Handle("GET "+prefix, pageHandler(prefix))
		root := strings.TrimSuffix(prefix, "/")
		mux.Handle("GET "+root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secure(w.Header())
			http.Redirect(w, r, prefix, http.StatusMovedPermanently)
		}))
	}
}

// Handler serves the embedded files under /ui/. A directory, an unknown
// file or any other extension is 404, never a listing.
func Handler() http.Handler { return pageHandler("/ui/") }

// pageHandler serves the embedded files under prefix, with the page's own
// HTML for the prefix itself; another page's HTML is 404.
func pageHandler(prefix string) http.Handler {
	index := pages[prefix]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w.Header())
		name, found := strings.CutPrefix(r.URL.Path, prefix)
		if name == "" {
			name = index
		}
		ctype := contentTypes[path.Ext(name)]
		if !found || ctype == "" || !fs.ValidPath(name) || (path.Ext(name) == ".html" && name != index) {
			notFound(w)
			return
		}
		body, err := fs.ReadFile(Files, name)
		if err != nil {
			notFound(w)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}
