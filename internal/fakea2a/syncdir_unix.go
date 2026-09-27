//go:build !windows

package fakea2a

import (
	"fmt"
	"os"
)

// syncDir makes a newly created log's directory entry durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("fakea2a: open data directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fakea2a: sync data directory: %w", err)
	}
	return nil
}
