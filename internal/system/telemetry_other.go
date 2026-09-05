//go:build !windows && !linux

package system

import (
	"runtime"
	"time"
)

func sampleOSMemory() MemoryMetrics {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return MemoryMetrics{
		ProcessRSS:       int64(ms.Sys),
		ProcessHeapAlloc: int64(ms.Alloc),
		ProcessHeapSys:   int64(ms.Sys),
	}
}

func sampleOSDisk(path string) DiskMetrics {
	return DiskMetrics{}
}

func sampleOSCPUTimes() (processNs, sysBusyNs, sysTotalNs int64, err error) {
	now := time.Now().UnixNano()
	return now, now, now, nil
}
