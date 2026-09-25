package ui_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"eacp/internal/ui"
)

// consoleFiles is every file the console serves. Adding a served file is a
// deliberate change: add it here too.
var consoleFiles = []string{
	"api.js", "app.css", "app.js", "confirm.js", "dom.js", "index.html", "router.js", "session.js",
	"views/common.js", "views/incidents.js", "views/overview.js",
}

var wantHeaders = map[string]string{
	"Content-Security-Policy":      ui.CSP,
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"X-Frame-Options":              "DENY",
	"Cache-Control":                "no-store",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=()",
}

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	ui.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	for k, v := range wantHeaders {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", path, k, got, v)
		}
	}
	return rec
}

func TestCSPIsStrict(t *testing.T) {
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'",
		"frame-ancestors 'none'", "form-action 'none'", "require-trusted-types-for 'script'"} {
		if !strings.Contains(ui.CSP, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	if strings.Contains(ui.CSP, "unsafe") {
		t.Errorf("CSP allows an unsafe source: %s", ui.CSP)
	}
}

func TestServesTheConsoleWithItsHeaders(t *testing.T) {
	cases := map[string]string{
		"/ui/":                  "text/html; charset=utf-8",
		"/ui/index.html":        "text/html; charset=utf-8",
		"/ui/app.js":            "text/javascript; charset=utf-8",
		"/ui/app.css":           "text/css; charset=utf-8",
		"/ui/views/overview.js": "text/javascript; charset=utf-8",
	}
	for path, ctype := range cases {
		rec := serve(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type = %q, want %q", path, got, ctype)
		}
	}
	if body := serve(t, "/ui/").Body.String(); !strings.Contains(body, `<script type="module" src="app.js"></script>`) {
		t.Errorf("index.html does not load app.js as a module:\n%s", body)
	}
}

func TestRedirectsToTheConsoleRoot(t *testing.T) {
	rec := serve(t, "/ui")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET /ui: %d %q, want 301 /ui/", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUnknownFilesAndDirectoriesAreNotFound(t *testing.T) {
	for _, path := range []string{"/ui/nope.js", "/ui/views", "/ui/views/", "/ui/package.json", "/ui/app.js.map",
		"/ui/jstest/api.test.mjs", "/ui/ui.go", "/ui/%2e%2e/ui.go"} {
		rec := httptest.NewRecorder()
		ui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ui.CSP {
			t.Errorf("%s: 404 lacks the CSP", path)
		}
	}
}

func TestEmbeddedFilesAreExactlyTheConsole(t *testing.T) {
	var got []string
	err := fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(consoleFiles)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("embedded files = %v\nwant %v", got, want)
	}
}
