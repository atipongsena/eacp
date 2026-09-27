//go:build !windows

package fakellm

import (
	"fmt"
	"os"
)

// syncDir makes a newly created audit log's directory entry durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fakellm: open data directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fakellm: sync data directory: %w", err)
	}
	return nil
}
