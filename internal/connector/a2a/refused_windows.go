package a2a

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED, which Windows reports for a refused
// connection instead of ECONNREFUSED.
const wsaeconnrefused = syscall.Errno(10061)

// refused reports a connection the target refused: nothing was sent.
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, wsaeconnrefused)
}
