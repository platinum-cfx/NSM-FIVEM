package main

// VPS-wide resource monitoring for the hosting panel. Values come from the
// real operating system counters of the machine the host process runs on —
// never invented. Anything the OS will not report stays at -1 and the panel
// shows an explicit "unavailable" state instead of a misleading number.

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type vpsUsage struct {
	CPUPct      float64 `json:"cpuPct"` // whole-VPS CPU utilisation, -1 unavailable
	RAMMB       int64   `json:"ramMB"`  // used memory
	RAMTotalMB  int64   `json:"ramTotalMB"`
	DiskMB      int64   `json:"diskMB"` // used space on the hosting volume
	DiskTotalMB int64   `json:"diskTotalMB"`
	Cores       int     `json:"cores"`
	UpdatedAt   int64   `json:"updatedAt"`
}

var (
	winPrevBusy  float64
	winPrevTotal float64

	vpsMu       sync.Mutex
	vpsMemo     vpsUsage
	vpsMemoAt   time.Time
	vpsPrevCPU  float64 = -1
	vpsPrevIdle float64 = -1
)

// hostVPSUsage samples the whole VPS with a short memo so dashboard polling
// stays cheap.
func hostVPSUsage() vpsUsage {
	vpsMu.Lock()
	if time.Since(vpsMemoAt) < 4*time.Second {
		u := vpsMemo
		vpsMu.Unlock()
		return u
	}
	vpsMu.Unlock()

	u := vpsUsage{CPUPct: -1, RAMMB: -1, RAMTotalMB: -1, DiskMB: -1, DiskTotalMB: -1, Cores: runtime.NumCPU(), UpdatedAt: time.Now().Unix()}

	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/stat"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "cpu ") {
					f := strings.Fields(line)
					if len(f) >= 5 {
						total, idle := 0.0, 0.0
						for i := 1; i < len(f); i++ {
							v, _ := strconv.ParseFloat(f[i], 64)
							total += v
							if i == 4 {
								idle = v
							}
						}
						if vpsPrevCPU >= 0 && total > vpsPrevCPU {
							dTotal := total - vpsPrevCPU
							dBusy := dTotal - (idle - vpsPrevIdle)
							if dTotal > 0 {
								u.CPUPct = dBusy / dTotal * 100
								if u.CPUPct < 0 {
									u.CPUPct = 0
								}
							}
						}
						vpsPrevCPU, vpsPrevIdle = total, idle
					}
					break
				}
			}
		}
		if b, err := os.ReadFile("/proc/meminfo"); err == nil {
			var totalKB, availKB int64
			for _, line := range strings.Split(string(b), "\n") {
				switch {
				case strings.HasPrefix(line, "MemTotal:"):
					f := strings.Fields(line)
					if len(f) >= 2 {
						totalKB, _ = strconv.ParseInt(f[1], 10, 64)
					}
				case strings.HasPrefix(line, "MemAvailable:"):
					f := strings.Fields(line)
					if len(f) >= 2 {
						availKB, _ = strconv.ParseInt(f[1], 10, 64)
					}
				}
			}
			if totalKB > 0 {
				u.RAMTotalMB = totalKB / 1024
				u.RAMMB = (totalKB - availKB) / 1024
			}
		}
	} else if runtime.GOOS == "windows" {
		if pct := winSystemCPU(&winPrevBusy, &winPrevTotal); pct >= 0 {
			u.CPUPct = pct
		}
		if used, total := winSystemMemory(); total > 0 {
			u.RAMMB, u.RAMTotalMB = used, total
		}
	}
	if du, dt, ok := hostVolumeUsage(); ok {
		u.DiskMB, u.DiskTotalMB = du, dt
	}

	vpsMu.Lock()
	vpsMemo = u
	vpsMemoAt = time.Now()
	vpsMu.Unlock()
	return u
}

func runWmic(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "wmic", args...).Output()
}

func wmicLastRow(out []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil
	}
	return strings.Split(strings.TrimSpace(lines[len(lines)-1]), ",")
}

func wmicLastInt(out []byte) int64 {
	parts := wmicLastRow(out)
	if len(parts) < 2 {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return -1
	}
	return v
}
