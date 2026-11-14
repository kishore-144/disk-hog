//go:build unix

package fsutil

import (
	"io/fs"
	"syscall"
)

// MetaOf extracts allocation and identity from an Lstat result.
func MetaOf(fi fs.FileInfo) Meta {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Meta{Alloc: fi.Size()}
	}
	return Meta{
		Dev:   uint64(st.Dev),
		Ino:   uint64(st.Ino),
		Nlink: uint64(st.Nlink),
		Alloc: int64(st.Blocks) * 512,
		HasID: true,
	}
}
