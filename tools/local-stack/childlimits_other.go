//go:build !linux && !windows

package main

// Other platforms: the configured maximums are recorded, monitored and shown,
// but the OS offers no safe per-process enforcement hook here.

func hostVolumeUsage() (int64, int64, bool) {
	return -1, -1, false
}

func applyChildLimits(pid int, l hostLimits) string {
	return ""
}
