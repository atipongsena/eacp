//go:build !windows

package fakeerp

import (
	"fmt"
	"os"
)

// syncDir makes a newly created operation log's directory entry durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fakeerp: open data directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fakeerp: sync data directory: %w", err)
	}
	return nil
}
