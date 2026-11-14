//go:build !unix && !windows

package fsutil

import "io/fs"

// MetaOf falls back to the apparent size on platforms without Stat_t.
func MetaOf(fi fs.FileInfo) Meta { return Meta{Alloc: fi.Size()} }
