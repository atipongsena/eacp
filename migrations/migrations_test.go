package migrations

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEveryMigrationHasUpAndDown(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations embedded (err=%v)", err)
	}
	for _, f := range files {
		b, err := fs.ReadFile(FS, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		s := string(b)
		if !strings.Contains(s, "-- +goose Up") || !strings.Contains(s, "-- +goose Down") {
			t.Errorf("%s: missing goose Up or Down section", f)
		}
	}
}

func TestLatestIsHighestEmbeddedVersion(t *testing.T) {
	files, _ := fs.Glob(FS, "*.sql")
	if got, want := Latest(), int64(len(files)); got != want {
		t.Fatalf("Latest() = %d, want %d (migrations must be numbered 1..N without gaps)", got, want)
	}
}
