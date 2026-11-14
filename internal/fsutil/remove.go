package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// RemoveAll deletes p and everything below it. Trees the owner cannot write
// to, such as the Go module cache which is deliberately read-only, are made
// writable and retried.
func RemoveAll(p string) error {
	err := os.RemoveAll(p)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		// Windows reports read-only files as plain access errors too.
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			return err
		}
	}
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if d == nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// RemoveContents empties dir but keeps it. It continues past entries it
// cannot remove (files in use on Windows) and returns the first error.
func RemoveContents(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var first error
	for _, e := range ents {
		if err := RemoveAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}
