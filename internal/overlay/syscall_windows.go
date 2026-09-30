//go:build windows

package overlay

import (
	"errors"
	"syscall"
)

// newCallback wraps syscall.NewCallback. Callbacks created this way are never
// released by the runtime, which is why exactly one is created per process.
func newCallback(fn func(hwnd, msg, wp, lp uintptr) uintptr) uintptr {
	return syscall.NewCallback(fn)
}

// ERROR_CLASS_ALREADY_EXISTS is benign: a previous Run in this process already
// registered the window class.
const errClassAlreadyExists = syscall.Errno(1410)

func isClassAlreadyExists(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == errClassAlreadyExists
}
