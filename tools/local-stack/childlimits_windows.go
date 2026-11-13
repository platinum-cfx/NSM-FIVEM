//go:build windows

package main

// Real per-process enforcement of the configured maximums on Windows hosts
// (the NSM production platform): a Job Object caps the server process at the
// configured memory maximum, and the process affinity mask is limited to the
// configured core count.

import (
	"fmt"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject     = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJob  = kernel32.NewProc("AssignProcessToJobObject")
	procSetInformationJob   = kernel32.NewProc("SetInformationJobObject")
	procOpenProcess         = kernel32.NewProc("OpenProcess")
	procSetProcessAffinity  = kernel32.NewProc("SetProcessAffinityMask")
)

const (
	jobObjectExtLimitInfoClass = 9
	jobObjectMemoryLimit              = 0x00002000
	processSetQuota                   = 0x0100
	processSetInformation             = 0x0200
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// hostVolumeUsage reports used/total MB of the C: volume (the VPS system
// drive that hosts the cities).
func hostVolumeUsage() (int64, int64, bool) {
	out, err := runWmic("logicaldisk", "where", "DeviceID=\"C:\"", "get", "FreeSpace,Size", "/format:csv")
	if err != nil {
		return -1, -1, false
	}
	parts := wmicLastRow(out)
	if len(parts) < 3 {
		return -1, -1, false
	}
	freeB, _ := strconv.ParseInt(parts[1], 10, 64)
	sizeB, _ := strconv.ParseInt(parts[2], 10, 64)
	if sizeB <= 0 {
		return -1, -1, false
	}
	return (sizeB - freeB) / (1 << 20), sizeB / (1 << 20), true
}

func applyChildLimits(pid int, l hostLimits) string {
	applied := ""
	hProc, _, _ := procOpenProcess.Call(processSetQuota|processSetInformation, 0, uintptr(pid))
	if hProc == 0 {
		return ""
	}
	if l.RAMMB > 0 {
		hJob, _, _ := procCreateJobObject.Call(0, 0)
		if hJob != 0 {
			info := jobObjectExtendedLimitInformation{}
			info.BasicLimitInformation.LimitFlags = jobObjectMemoryLimit
			info.ProcessMemoryLimit = uintptr(l.RAMMB) << 20
			if r, _, _ := procSetInformationJob.Call(hJob, jobObjectExtLimitInfoClass, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r != 0 {
				if r2, _, _ := procAssignProcessToJob.Call(hJob, hProc); r2 != 0 {
					applied = fmt.Sprintf("memory cap %d MB", l.RAMMB)
				}
			}
		}
	}
	if l.CPUCores > 0 && l.CPUCores < runtime.NumCPU() {
		var mask uintptr = (1 << uintptr(l.CPUCores)) - 1
		if r, _, _ := procSetProcessAffinity.Call(hProc, uintptr(unsafe.Pointer(&mask))); r != 0 {
			if applied != "" {
				applied += ", "
			}
			applied += fmt.Sprintf("cpu cap %d cores", l.CPUCores)
		}
	}
	return applied
}
