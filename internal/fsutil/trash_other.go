//go:build !unix && !windows

package fsutil

import "errors"

// Trash is unavailable on this platform.
func Trash(string) error { return errors.New("trash not supported on this platform; use delete") }
