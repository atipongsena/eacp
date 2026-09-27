//go:build !windows

package a2a

import (
	"errors"
	"syscall"
)

// refused reports a connection the target refused: nothing was sent.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
