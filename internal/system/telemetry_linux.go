//go:build linux

package system

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func sampleOSMemory() MemoryMetrics {
	var out MemoryMetrics

	// 1. Host System RAM via /proc/meminfo
	if f, err := os.Open("/proc/meminfo"); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			val, _ := strconv.ParseInt(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				out.SystemTotal = val * 1024
			case "MemAvailable:":
				out.SystemFree = val * 1024
			}
		}
		f.Close()
		if out.SystemTotal > 0 && out.SystemFree >= 0 {
			out.SystemUsed = out.SystemTotal - out.SystemFree
		}
	}

	// 2. Process RSS via /proc/self/statm
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 2 {
			if rssPages, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				out.ProcessRSS = rssPages * int64(os.Getpagesize())
			}
		}
	}

	return out
}

func sampleOSDisk(path string) DiskMetrics {
	if path == "" {
		path = "."
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskMetrics{}
	}
	// Blocks * Bsize gives total drive capacity
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return DiskMetrics{
		TotalBytes: total,
		FreeBytes:  free,
	}
}

func sampleOSCPUTimes() (processNs, sysBusyNs, sysTotalNs int64, err error) {
	// Clock tick resolution in Linux is 100 Hz (10 ms per tick)
	const nsPerTick = 10_000_000

	// 1. Process CPU times via /proc/self/stat
	if raw, err := os.ReadFile("/proc/self/stat"); err == nil {
		content := string(raw)
		if idx := strings.LastIndex(content, ")"); idx != -1 && idx+2 < len(content) {
			fields := strings.Fields(content[idx+2:])
			// In fields after ')', field index 11 is utime and 12 is stime (0-indexed)
			if len(fields) >= 13 {
				utime, _ := strconv.ParseInt(fields[11], 10, 64)
				stime, _ := strconv.ParseInt(fields[12], 10, 64)
				processNs = (utime + stime) * nsPerTick
			}
		}
	}

	// 2. System CPU times via /proc/stat
	if f, err := os.Open("/proc/stat"); err == nil {
		scanner := bufio.NewScanner(f)
		if scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "cpu ") {
				fields := strings.Fields(line[4:])
				if len(fields) >= 4 {
					var totalTicks, idleTicks int64
					for i, s := range fields {
						val, _ := strconv.ParseInt(s, 10, 64)
						totalTicks += val
						if i == 3 || i == 4 { // idle or iowait
							idleTicks += val
						}
					}
					busyTicks := totalTicks - idleTicks
					if busyTicks < 0 {
						busyTicks = 0
					}
					sysBusyNs = busyTicks * nsPerTick
					sysTotalNs = totalTicks * nsPerTick
				}
			}
		}
		f.Close()
	}

	return processNs, sysBusyNs, sysTotalNs, nil
}
