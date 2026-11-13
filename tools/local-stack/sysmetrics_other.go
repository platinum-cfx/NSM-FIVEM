//go:build !windows

package main

// Stubs so the platform-agnostic code compiles on non-Windows hosts; the
// real implementations live in sysmetrics_windows.go and only run on the
// Windows production platform.

func winProcUsage(pid int, p *hostedProc) (float64, int64) { return -1, -1 }

func winSystemCPU(prevBusy, prevTotal *float64) float64 { return -1 }

func winSystemMemory() (int64, int64) { return -1, -1 }

func winVolumeUsage(path string) (int64, int64) { return -1, -1 }
