package ui_test

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/api"
	"github.com/atipongsena/eacp/internal/ui"
)

var (
	routeRE   = regexp.MustCompile(`\[\s*'([a-z][a-z.]*)',\s*'(GET|POST|PUT|DELETE)',\s*'(/v1/[^']*)'`)
	literalRE = regexp.MustCompile(`'(/v1/[^'?]*)`)
	callRE    = regexp.MustCompile(`\.call\('([a-z][a-z.]*)'`)
	paramRE   = regexp.MustCompile(`\{[a-z]+\}`)
)

// TestEveryConsoleCallIsARealRoute proves the console never calls an
// invented route: every ROUTES entry resolves on the real API mux to the
// same pattern, every /v1 literal is a ROUTES path, and every call names a
// ROUTES entry.
func TestEveryConsoleCallIsARealRoute(t *testing.T) {
	src, err := fs.ReadFile(ui.Files, "api.js")
	if err != nil {
		t.Fatal(err)
	}
	routes := routeRE.FindAllStringSubmatch(string(src), -1)
	if len(routes) < 30 {
		t.Fatalf("found %d routes in api.js; the pattern no longer matches the table", len(routes))
	}
	mux := http.NewServeMux()
	api.New(nil, slog.New(slog.DiscardHandler)).Register(mux)
	names, paths := map[string]bool{}, map[string]bool{}
	for _, m := range routes {
		name, method, tmpl := m[1], m[2], m[3]
		if names[name] {
			t.Errorf("route %s is listed twice", name)
		}
		names[name], paths[tmpl] = true, true
		concrete := paramRE.ReplaceAllStringFunc(tmpl, func(p string) string {
			if p == "{ref}" {
				return "sample-agent"
			}
			return "0b5e3c1a-7f2d-4c8e-9a61-3d2f5e7a9b10"
		})
		_, pattern := mux.Handler(httptest.NewRequest(method, concrete, nil))
		if want := method + " " + tmpl; pattern != want {
			t.Errorf("%s: %s %s resolves to %q, want %q", name, method, concrete, pattern, want)
		}
	}
	err = fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, err := fs.ReadFile(ui.Files, path)
		for _, m := range literalRE.FindAllStringSubmatch(string(b), -1) {
			if !paths[m[1]] {
				t.Errorf("%s uses %s, which is not in ROUTES", path, m[1])
			}
		}
		for _, m := range callRE.FindAllStringSubmatch(string(b), -1) {
			if !names[m[1]] {
				t.Errorf("%s calls %q, which is not in ROUTES", path, m[1])
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
