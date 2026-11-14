//go:build darwin || freebsd || dragonfly

package fsutil

import (
	"os"
	"path/filepath"
	"syscall"
)

// Disks reports the root filesystem plus anything mounted under /Volumes
// (macOS) or /mnt and /media (BSD).
func Disks() []Disk {
	paths := []string{"/"}
	for _, glob := range []string{"/Volumes/*", "/mnt/*", "/media/*"} {
		m, _ := filepath.Glob(glob)
		paths = append(paths, m...)
	}
	seen := map[uint64]bool{}
	var out []Disk
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if seen[uint64(st.Dev)] {
				continue
			}
			seen[uint64(st.Dev)] = true
		}
		var s syscall.Statfs_t
		if syscall.Statfs(p, &s) != nil || s.Blocks == 0 {
			continue
		}
		bs := int64(s.Bsize)
		d := Disk{Mount: p, Total: int64(s.Blocks) * bs, Free: int64(s.Bfree) * bs, Avail: int64(s.Bavail) * bs}
		d.Used = d.Total - d.Free
		out = append(out, d)
	}
	return out
}
