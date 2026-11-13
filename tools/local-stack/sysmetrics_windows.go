//go:build windows

package main

// Real Windows performance counters for the hosting panel. These use the
// Win32 API directly — no shell tools — because WMIC is deprecated and
// removed on current Windows Server builds, which left CPU/RAM readings
// permanently unavailable on the production VPS.

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	k32                = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes  = k32.NewProc("GetSystemTimes")
	procGetProcessTimes = k32.NewProc("GetProcessTimes")
	procGlobalMemStatus = k32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeEx   = k32.NewProc("GetDiskFreeSpaceExW")
	procCloseHandle     = k32.NewProc("CloseHandle")
	psapi              = syscall.NewLazyDLL("psapi.dll")
	procGetProcMemInfo = psapi.NewProc("GetProcessMemoryInfo")
)

const processQueryInformation = 0x0400

type winFileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (t winFileTime) ticks() uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

type winProcessMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type winMemoryStatusEx struct {
	dwLength     uint32
	dwMemoryLoad uint32
	ullTotalPhys uint64
	ullAvailPhys uint64
	ullTotalPage uint64
	ullAvailPage uint64
	ullTotalVirt uint64
	ullAvailVirt uint64
	ullAvailExt  uint64
}

// winProcUsage returns the process CPU percentage (of one core × 100) and
// working-set MB for pid. cpu is -1 when the OS will not report it.
// The caller owns the sample bookkeeping in p.lastCPU/p.lastTime.
func winProcUsage(pid int, p *hostedProc) (float64, int64) {
	cpu := -1.0
	ram := int64(-1)
	// OpenProcess lives in childlimits_windows.go (same package).
	hProc, _, _ := procOpenProcess.Call(processQueryInformation, 0, uintptr(pid))
	if hProc == 0 {
		return cpu, ram
	}
	defer procCloseHandle.Call(hProc)

	var creation, exit, kernel, user winFileTime
	if r, _, _ := procGetProcessTimes.Call(hProc, uintptr(unsafe.Pointer(&creation)), uintptr(unsafe.Pointer(&exit)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r != 0 {
		ticks := float64(kernel.ticks() + user.ticks()) // 100ns units
		elapsed := time.Since(p.lastTime).Seconds()
		if p.lastCPU >= 0 && elapsed > 0.5 {
			cpu = ((ticks - p.lastCPU) / 1e7) / elapsed * 100
			if cpu < 0 {
				cpu = 0
			}
		}
		p.lastCPU = ticks
	}
	var pmc winProcessMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procGetProcMemInfo.Call(hProc, uintptr(unsafe.Pointer(&pmc)), uintptr(unsafe.Sizeof(pmc))); r != 0 {
		ram = int64(pmc.WorkingSetSize) / (1 << 20)
	}
	return cpu, ram
}

// winSystemCPU returns whole-machine CPU utilisation percentage using the
// system idle/kernel/user times, or -1 when unavailable.
func winSystemCPU(prevBusy, prevTotal *float64) float64 {
	var idle, kernel, user winFileTime
	if r, _, _ := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return -1
	}
	total := float64(kernel.ticks()+user.ticks()+idle.ticks()) / 1e7
	busy := float64(kernel.ticks()+user.ticks()) / 1e7
	defer func() { *prevBusy, *prevTotal = busy, total }()
	if *prevTotal > 0 && total > *prevTotal {
		dTotal := total - *prevTotal
		dBusy := busy - *prevBusy
		pct := dBusy / dTotal * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		return pct
	}
	return -1 // first sample: no delta yet
}

// winSystemMemory returns used/total physical memory in MB, or -1s.
func winSystemMemory() (int64, int64) {
	var ms winMemoryStatusEx
	ms.dwLength = uint32(unsafe.Sizeof(ms))
	if r, _, _ := procGlobalMemStatus.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return -1, -1
	}
	total := int64(ms.ullTotalPhys) / (1 << 20)
	used := (int64(ms.ullTotalPhys) - int64(ms.ullAvailPhys)) / (1 << 20)
	return used, total
}

// winVolumeUsage returns used/total MB of the given volume path (e.g. "C:").
func winVolumeUsage(path string) (int64, int64) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1, -1
	}
	var freeBytes, totalBytes, totalFree uint64
	if r, _, _ := procGetDiskFreeEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&freeBytes)), uintptr(unsafe.Pointer(&totalBytes)), uintptr(unsafe.Pointer(&totalFree))); r == 0 {
		return -1, -1
	}
	total := int64(totalBytes) / (1 << 20)
	used := (int64(totalBytes) - int64(freeBytes)) / (1 << 20)
	return used, total
}
