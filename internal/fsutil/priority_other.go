//go:build !linux

package fsutil

// LowerPriority is best effort; outside Linux the service manager
// (launchd's LowPriorityIO, Windows task priority) handles it.
func LowerPriority() {}
