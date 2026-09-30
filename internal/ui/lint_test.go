package ui_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/ui"
)

// banned are sinks and APIs the console must never use (ADR-028): markup
// parsing, code from strings, browser storage for the key, cookies, other
// origins and dynamic imports. Comments count too.
var banned = []string{
	"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "Function(",
	"setTimeout('", `setTimeout("`, "setInterval('", `setInterval("`, "srcdoc", "javascript:", "localStorage",
	"sessionStorage", "indexedDB", "document.cookie", "http://", "https://", "DOMParser", "createContextualFragment",
	"import(", "`/v1",
}

func consoleSource(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".css") {
			return err
		}
		b, err := fs.ReadFile(ui.Files, path)
		out[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConsoleUsesNoDangerousSinks(t *testing.T) {
	for path, src := range consoleSource(t) {
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Errorf("%s contains %q", path, b)
			}
		}
		if strings.Contains(src, "fetch(") && path != "api.js" && path != "session.js" {
			t.Errorf("%s calls fetch; only api.js and session.js may", path)
		}
	}
}

func TestEveryCallNamesItsRouteLiterally(t *testing.T) {
	any := regexp.MustCompile(`\.call\(`)
	literal := regexp.MustCompile(`\.call\('[a-z][a-z.]*'`)
	for path, src := range consoleSource(t) {
		if n, m := len(any.FindAllString(src, -1)), len(literal.FindAllString(src, -1)); n != m {
			t.Errorf("%s: %d .call( uses, %d with a literal route name", path, n, m)
		}
	}
}

// TestIndexLoadsOnlyTheConsole checks both pages: the console loads only
// app.js, the Studio page only studio.js, and both only app.css.
func TestIndexLoadsOnlyTheConsole(t *testing.T) {
	for page, script := range map[string]string{"index.html": "app.js", "studio.html": "studio.js"} {
		src := consoleSource(t)[page]
		if src == "" {
			t.Fatalf("%s is missing", page)
		}
		if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(src) {
			t.Errorf("%s has an inline event handler", page)
		}
		if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(src) {
			t.Errorf("%s has an inline style", page)
		}
		scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(src, -1)
		if len(scripts) != 1 {
			t.Errorf("%s has %d scripts, want 1", page, len(scripts))
		}
		for _, m := range scripts {
			if strings.TrimSpace(m[2]) != "" || !strings.Contains(m[1], `src="`+script+`"`) {
				t.Errorf("%s: inline or foreign script: %s", page, m[0])
			}
		}
		for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(src, -1) {
			if ref := m[1]; !strings.HasPrefix(ref, "#/") && ref != script && ref != "app.css" {
				t.Errorf("%s references %q", page, ref)
			}
		}
	}
}
