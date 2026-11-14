//go:build !linux && !darwin && !freebsd && !dragonfly && !windows

package fsutil

// Disks is not implemented on this platform.
func Disks() []Disk { return nil }
