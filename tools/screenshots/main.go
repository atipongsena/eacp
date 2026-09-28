// Command screenshots signs in to the operator console of a running EACP
// stack and saves one PNG per view, for the README. It is its own module so
// that chromedp never enters the main module. Run it through
// scripts/screenshots.sh, which prepares the data the views show.
//
// Each shot is NAME:KEY_ENV:HASH, for example
// overview:OPERATOR_KEY:#/overview. KEY_ENV names the environment variable
// that holds the key: a key is never a flag or an argument, and never
// printed. Shots named in -full capture the whole page, not the viewport.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

type shot struct{ name, keyEnv, hash string }

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	keyEnvRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

func parseShot(arg string) (shot, error) {
	parts := strings.SplitN(arg, ":", 3)
	if len(parts) != 3 || !nameRE.MatchString(parts[0]) || !keyEnvRE.MatchString(parts[1]) ||
		!strings.HasPrefix(parts[2], "#/") {
		return shot{}, fmt.Errorf("shot %q: want NAME:KEY_ENV:#/route", arg)
	}
	return shot{parts[0], parts[1], parts[2]}, nil
}

func main() {
	api := flag.String("api", "http://localhost:8080", "the control plane API; the console is at /ui/")
	out := flag.String("out", "docs/images", "directory for the PNG files")
	browser := flag.String("chrome", "", "Chrome or Edge executable (default: chromedp's search)")
	width := flag.Int("width", 1440, "viewport width")
	height := flag.Int("height", 900, "viewport height")
	full := flag.String("full", "", "comma-separated shot names to capture as the whole page")
	flag.Parse()
	whole := map[string]bool{}
	for _, n := range strings.Split(*full, ",") {
		if n != "" {
			whole[n] = true
		}
	}
	if err := run(*api, *out, *browser, *width, *height, whole, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "screenshots:", err)
		os.Exit(1)
	}
}

func run(api, out, browser string, width, height int, whole map[string]bool, args []string) error {
	if len(args) == 0 {
		return errors.New("no shots; each argument is NAME:KEY_ENV:#/route")
	}
	var shots []shot
	for _, a := range args {
		s, err := parseShot(a)
		if err != nil {
			return err
		}
		if os.Getenv(s.keyEnv) == "" {
			return fmt.Errorf("shot %s: %s is not set", s.name, s.keyEnv)
		}
		shots = append(shots, s)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.WindowSize(width, height))
	if browser != "" {
		opts = append(opts, chromedp.ExecPath(browser))
	}
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// One browser tab per key: the console keeps the key in the tab's
	// memory, so every view is reached by changing the hash, never by
	// loading the page again.
	var tab context.Context
	signedIn := ""
	for _, s := range shots {
		if s.keyEnv != signedIn {
			var cancelTab context.CancelFunc
			tab, cancelTab = chromedp.NewContext(allocCtx)
			defer cancelTab()
			// The first Run starts the browser and ties it to the context it
			// is given: start it on the tab, not on a step's timeout.
			if err := chromedp.Run(tab); err != nil {
				return fmt.Errorf("start the browser: %w", err)
			}
			if err := signIn(tab, api, os.Getenv(s.keyEnv), width, height); err != nil {
				return fmt.Errorf("sign in with %s: %w", s.keyEnv, err)
			}
			signedIn = s.keyEnv
		}
		png, err := capture(tab, s.hash, whole[s.name])
		if err != nil {
			return fmt.Errorf("shot %s: %w", s.name, err)
		}
		path := filepath.Join(out, "console-"+s.name+".png")
		if err := os.WriteFile(path, png, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return nil
}

func signIn(tab context.Context, api, key string, width, height int) error {
	ctx, cancel := context.WithTimeout(tab, 30*time.Second)
	defer cancel()
	var ok bool
	return chromedp.Run(ctx,
		chromedp.EmulateViewport(int64(width), int64(height)),
		chromedp.Navigate(strings.TrimRight(api, "/")+"/ui/"),
		chromedp.WaitVisible(`#key`, chromedp.ByQuery),
		chromedp.SendKeys(`#key`, key, chromedp.ByQuery),
		chromedp.Click(`form.signin button[type=submit]`, chromedp.ByQuery),
		chromedp.Poll(`!document.body.classList.contains('signed-out') || !!document.querySelector('#main .notice.error')`,
			&ok, chromedp.WithPollingTimeout(20*time.Second)),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var failed bool
			if err := chromedp.Evaluate(`!!document.querySelector('#main .notice.error')`, &failed).Do(ctx); err != nil {
				return err
			}
			if failed {
				return errors.New("the console refused the key")
			}
			return nil
		}),
	)
}

// capture empties the view, moves to hash, waits until the console has
// rendered it (the view's heading is in #main and the page is not busy),
// fails on an error notice and returns a PNG of the viewport, or of the
// whole page when full is set.
func capture(tab context.Context, hash string, full bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(tab, 45*time.Second)
	defer cancel()
	var (
		done   bool
		failed string
		png    []byte
	)
	err := chromedp.Run(ctx,
		chromedp.Evaluate(fmt.Sprintf(`document.getElementById('main').replaceChildren(); location.hash = %q; true`, hash), &done),
		chromedp.Poll(`(() => { const m = document.getElementById('main');
			return !m.hasAttribute('aria-busy') && !!m.querySelector('h1, h2'); })()`,
			&done, chromedp.WithPollingTimeout(30*time.Second)),
		// Let fonts and layout settle.
		chromedp.Sleep(700*time.Millisecond),
		chromedp.Evaluate(`(document.querySelector('#main .notice.error') || {}).textContent || ''`, &failed),
		chromedp.ActionFunc(func(context.Context) error {
			if failed != "" {
				return fmt.Errorf("the page shows an error: %s", failed)
			}
			return nil
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if full {
				return chromedp.FullScreenshot(&png, 100).Do(ctx)
			}
			return chromedp.CaptureScreenshot(&png).Do(ctx)
		}),
	)
	return png, err
}
