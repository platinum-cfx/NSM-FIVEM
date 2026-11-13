//go:build linux

package main

// Real per-process enforcement of the configured maximums on Linux hosts:
// RLIMIT_AS caps address space at the configured memory limit and
// sched_setaffinity pins the process to at most the configured core count.

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// prlimit64 sets/reads another process's rlimits (syscall.Prlimit lives in
// x/sys, so we speak the raw syscall).
func prlimit64(pid int, resource int, newLim, oldLim *syscall.Rlimit) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(pid), uintptr(resource), uintptr(unsafe.Pointer(newLim)), uintptr(unsafe.Pointer(oldLim)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// hostVolumeUsage reports used/total MB of the volume holding the working
// directory (the hosting root).
func hostVolumeUsage() (int64, int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(".", &st); err != nil {
		return -1, -1, false
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize / (1 << 20)
	used := (int64(st.Blocks) - int64(st.Bfree)) * bsize / (1 << 20)
	return used, total, true
}

func applyChildLimits(pid int, l hostLimits) string {
	if l.RAMMB <= 0 && l.CPUCores <= 0 {
		return ""
	}
	applied := ""
	if l.RAMMB > 0 {
		if err := prlimit64(pid, syscall.RLIMIT_AS, &syscall.Rlimit{Cur: uint64(l.RAMMB) << 20, Max: uint64(l.RAMMB) << 20}, nil); err == nil {
			applied = fmt.Sprintf("memory cap %d MB", l.RAMMB)
		}
	}
	if l.CPUCores > 0 && l.CPUCores < runtime.NumCPU() {
		if setAffinityCores(pid, l.CPUCores) == nil {
			if applied != "" {
				applied += ", "
			}
			applied += fmt.Sprintf("cpu cap %d cores", l.CPUCores)
		}
	}
	return applied
}

// setAffinityCores pins pid to the first n logical CPUs (Linux only).
func setAffinityCores(pid, n int) error {
	if n <= 0 {
		return nil
	}
	var mask [16]uint64 // up to 1024 logical CPUs
	for i := 0; i < n && i < 1024; i++ {
		mask[i/64] |= 1 << (uint(i) % 64)
	}
	_, _, errno := syscall.RawSyscall6(syscall.SYS_SCHED_SETAFFINITY, uintptr(pid), uintptr(unsafe.Sizeof(mask)), uintptr(unsafe.Pointer(&mask[0])), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
