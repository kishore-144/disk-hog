//go:build linux

package fsutil

import (
	"os"
	"strconv"
	"syscall"
)

// LowerPriority makes the process a polite background citizen: nice 10 and
// the idle I/O class, so a scan never competes with an editor or a build.
//
// Both settings are per thread on Linux, so every existing thread is
// updated; threads the Go runtime creates later inherit from their creator.
func LowerPriority() {
	const (
		ioprioWhoProcess = 1
		ioprioClassIdle  = 3
		ioprioClassShift = 13
	)
	tids := []int{0}
	if d, err := os.ReadDir("/proc/self/task"); err == nil {
		tids = tids[:0]
		for _, e := range d {
			if t, err := strconv.Atoi(e.Name()); err == nil {
				tids = append(tids, t)
			}
		}
	}
	for _, t := range tids {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, t, 10)
		syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(t), ioprioClassIdle<<ioprioClassShift)
	}
}
