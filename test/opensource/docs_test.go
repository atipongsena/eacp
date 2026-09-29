package opensource

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// markdownFiles lists every tracked markdown file.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range trackedFiles(t) {
		if strings.HasSuffix(f, ".md") {
			out = append(out, f)
		}
	}
	return out
}

// pairs returns each Thai document with its English original.
func pairs(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	for _, f := range markdownFiles(t) {
		if en, ok := strings.CutSuffix(f, ".th.md"); ok {
			out = append(out, [2]string{en + ".md", f})
		}
	}
	return out
}

var (
	heading = regexp.MustCompile(`^(#{1,6})\s`)
	image   = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)`)
	link    = regexp.MustCompile(`\]\(([^)\s]+)`)
	span    = regexp.MustCompile("`[^`]*`")
)

// outline is a document's shape: its heading levels, a "fence" per code
// fence and "img:<target>" per image, in order. Two translations of one
// document have the same outline.
func outline(md string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				out = append(out, "fence")
			}
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := heading.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
		for _, m := range image.FindAllStringSubmatch(span.ReplaceAllString(line, ""), -1) {
			out = append(out, "img:"+m[1])
		}
	}
	return out
}

func TestOutlineSeesHeadingsFencesAndImages(t *testing.T) {
	en := "# A\n\n## B\n\n![x](a.png)\n\n```sh\n# not a heading\n```\n"
	for _, tc := range []struct {
		name, th string
		same     bool
	}{
		{"translated", "# \u0e01\n\n## \u0e02\n\n![\u0e20\u0e32\u0e1e](a.png)\n\n```sh\n# \u0e44\u0e21\u0e48\u0e43\u0e0a\u0e48\u0e2b\u0e31\u0e27\u0e02\u0e49\u0e2d\n```\n", true},
		{"a heading missing", "# \u0e01\n\n![\u0e20\u0e32\u0e1e](a.png)\n\n```sh\n```\n", false},
		{"an extra image", "# \u0e01\n\n## \u0e02\n\n![\u0e20\u0e32\u0e1e](a.png)\n![\u0e20\u0e32\u0e1e](b.png)\n\n```sh\n```\n", false},
		{"a fence missing", "# \u0e01\n\n## \u0e02\n\n![\u0e20\u0e32\u0e1e](a.png)\n", false},
	} {
		if got := slices.Equal(outline(en), outline(tc.th)); got != tc.same {
			t.Errorf("%s: outlines equal = %v, want %v (%v vs %v)", tc.name, got, tc.same, outline(en), outline(tc.th))
		}
	}
}

// languageSwitch is the first line of both files of a pair.
func languageSwitch(en, th string) string {
	return "[English](" + path.Base(en) + ") | [ไทย](" + path.Base(th) + ")"
}

func TestTranslationsMatch(t *testing.T) {
	for _, p := range pairs(t) {
		en, th := p[0], p[1]
		if _, err := os.Stat(filepath.Join(repoRoot(t), filepath.FromSlash(en))); err != nil {
			t.Errorf("%s has no English original %s", th, en)
			continue
		}
		enText, thText := read(t, en), read(t, th)
		want := languageSwitch(en, th)
		for f, text := range map[string]string{en: enText, th: thText} {
			if first, _, _ := strings.Cut(text, "\n"); strings.TrimSpace(first) != want {
				t.Errorf("%s: the first line is %q, want the language switch %q", f, first, want)
			}
		}
		if a, b := outline(enText), outline(thText); !slices.Equal(a, b) {
			t.Errorf("%s and %s differ in shape (headings, code blocks, images):\n  en %v\n  th %v", en, th, a, b)
		}
	}
}

func isThai(r rune) bool { return r >= 0x0E00 && r <= 0x0E7F }

// thaiProductFiles are the product's own Thai texts, not documents: the console's
// Thai message catalogue (ADR-028 Rev 1.1). Each is named exactly; a new one
// needs a decision, not a pattern.
var thaiProductFiles = map[string]bool{"internal/ui/static/messages.th.js": true}

func TestThaiOnlyInThaiFiles(t *testing.T) {
	root := repoRoot(t)
	for _, f := range trackedFiles(t) {
		if strings.HasSuffix(f, ".th.md") || thaiProductFiles[f] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil || !isText(raw) {
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// The language label is the one Thai word allowed elsewhere.
			if strings.IndexFunc(strings.ReplaceAll(line, "ไทย", ""), isThai) >= 0 {
				t.Errorf("%s:%d has Thai text outside a .th.md file", f, i+1)
				break
			}
		}
	}
}

func TestMarkdownLinksResolve(t *testing.T) {
	root := repoRoot(t)
	for _, f := range markdownFiles(t) {
		inFence := false
		for i, line := range strings.Split(read(t, f), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range link.FindAllStringSubmatch(span.ReplaceAllString(line, ""), -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
					continue
				}
				target, _, _ = strings.Cut(target, "#")
				target, _, _ = strings.Cut(target, "?")
				rel := path.Join(path.Dir(f), target)
				if strings.HasPrefix(target, "/") {
					rel = strings.TrimPrefix(target, "/")
				}
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
					t.Errorf("%s:%d links to %s, which does not exist", f, i+1, m[1])
				}
			}
		}
	}
}

func TestReadmeNeverSaysPortfolio(t *testing.T) {
	for _, f := range []string{"README.md", "README.th.md"} {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), f))
		if err != nil {
			if f == "README.md" {
				t.Fatal(err)
			}
			continue
		}
		if strings.Contains(strings.ToLower(string(raw)), "portfolio") {
			t.Errorf("%s calls the project a portfolio", f)
		}
	}
}
