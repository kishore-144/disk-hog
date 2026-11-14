//go:build windows

package fsutil

import "io/fs"

// MetaOf extracts allocation from an Lstat result. Windows does not expose
// inode identity through os.FileInfo, so hard links count once per name and
// the one-filesystem check is skipped.
func MetaOf(fi fs.FileInfo) Meta {
	n := fi.Size()
	// Round up to the common 4 KiB cluster so many small files are not
	// under-reported.
	if n > 0 {
		n = (n + 4095) &^ 4095
	}
	return Meta{Alloc: n}
}
