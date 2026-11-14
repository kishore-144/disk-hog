//go:build windows

package fsutil

import (
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	procGetDriveType     = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpace = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// Disks lists fixed, removable and network drives.
func Disks() []Disk {
	mask, _, _ := procGetLogicalDrives.Call()
	var out []Disk
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := syscall.UTF16PtrFromString(root)
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p)))
		if t != 2 && t != 3 && t != 4 { // removable, fixed, remote
			continue
		}
		var avail, total, free uint64
		r, _, _ := procGetDiskFreeSpace.Call(uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
		if r == 0 || total == 0 {
			continue
		}
		out = append(out, Disk{Mount: root, Total: int64(total), Free: int64(free), Avail: int64(avail),
			Used: int64(total - free)})
	}
	return out
}
