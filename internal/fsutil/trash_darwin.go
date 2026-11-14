//go:build darwin

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// Trash moves p to ~/.Trash. Items on another volume cannot be renamed there
// and must be deleted instead.
func Trash(p string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".Trash")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Base(p)
	dst := filepath.Join(dir, base)
	for i := 2; ; i++ {
		if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
			break
		}
		dst = filepath.Join(dir, base+" "+strconv.Itoa(i))
	}
	return os.Rename(p, dst)
}
