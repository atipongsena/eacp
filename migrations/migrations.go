// Package migrations embeds the SQL schema migrations (goose format).
//
// Files are named NNNNN_description.sql and numbered 1..N without gaps.
package migrations

import (
	"embed"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// Latest returns the highest migration version embedded in this binary.
// Services require the database to be at least this version to be ready.
func Latest() int64 {
	files, _ := fs.Glob(FS, "*.sql")
	var latest int64
	for _, f := range files {
		prefix, _, _ := strings.Cut(f, "_")
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil && v > latest {
			latest = v
		}
	}
	return latest
}
