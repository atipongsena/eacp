package ui_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/atipongsena/eacp/internal/ui"
)

// The console speaks English and Thai (ADR-028 Rev 1.1). node checks the same
// rules richly; these checks run with `go test` alone, so a missing Thai text
// can never ship just because node was not on the machine.
var (
	tCall    = regexp.MustCompile(`(?:^|[^\w.$])t\(`)
	tLiteral = regexp.MustCompile(`(?:^|[^\w.$])t\('((?:[^'\\\n]|\\.)*)'`)
	catEntry = regexp.MustCompile(`(?m)^\s*'((?:[^'\\\n]|\\.)*)':`)
)

func unquote(s string) string { return strings.ReplaceAll(s, `\'`, `'`) }

func TestEveryTranslatedTextHasAThaiEntry(t *testing.T) {
	catalogue, err := fs.ReadFile(ui.Files, "messages.th.js")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range catEntry.FindAllStringSubmatch(string(catalogue), -1) {
		have[unquote(m[1])] = true
	}
	if len(have) < 100 {
		t.Fatalf("read %d catalogue entries; the pattern no longer matches messages.th.js", len(have))
	}
	used := map[string]bool{}
	err = fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") || path == "i18n.js" || path == "messages.th.js" {
			return err
		}
		b, err := fs.ReadFile(ui.Files, path)
		src := string(b)
		if calls, literal := len(tCall.FindAllString(src, -1)), len(tLiteral.FindAllString(src, -1)); calls != literal {
			t.Errorf("%s: %d t( calls, %d with a literal text", path, calls, literal)
		}
		for _, m := range tLiteral.FindAllStringSubmatch(src, -1) {
			key := unquote(m[1])
			used[key] = true
			if !have[key] {
				t.Errorf("%s: no Thai text for %q", path, key)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for key := range have {
		if !used[key] {
			t.Errorf("messages.th.js has %q, which no t() call uses", key)
		}
	}
}

// The language is never stored: no view may keep it in storage, a cookie or
// the URL fragment. ?lang= in the address is the only carrier.
func TestLanguageChoiceIsNotStored(t *testing.T) {
	src, err := fs.ReadFile(ui.Files, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "replaceState") || !strings.Contains(string(src), "searchParams.set('lang'") {
		t.Error("app.js does not carry the language in ?lang= with history.replaceState")
	}
}
