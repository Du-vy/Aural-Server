package system

import (
	"runtime"
	"runtime/metrics"
	"sync"
	"time"

	"github.com/aural-chat/aural-server/internal/buildinfo"
)

// CPUMetrics describes CPU utilization.
type CPUMetrics struct {
	ProcessPercent float64 `json:"processPercent"`
	SystemPercent  float64 `json:"systemPercent"`
	Cores          int     `json:"cores"`
}

// MemoryMetrics describes process and host memory usage in bytes.
type MemoryMetrics struct {
	ProcessRSS       int64   `json:"processRss"`
	ProcessHeapAlloc int64   `json:"processHeapAlloc"`
	ProcessHeapSys   int64   `json:"processHeapSys"`
	SystemTotal      int64   `json:"systemTotal"`
	SystemUsed       int64   `json:"systemUsed"`
	SystemFree       int64   `json:"systemFree"`
	SystemPercent    float64 `json:"systemPercent"`
}

// DiskMetrics describes the host storage drive capacity.
type DiskMetrics struct {
	TotalBytes int64 `json:"totalBytes"`
	FreeBytes  int64 `json:"freeBytes"`
}

// SystemInfo describes the server environment and runtime state.
type SystemInfo struct {
	UptimeSeconds int64  `json:"uptimeSeconds"`
	StartedAt     int64  `json:"startedAt"`
	Goroutines    int    `json:"goroutines"`
	GoVersion     string `json:"goVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	ServerVersion string `json:"serverVersion"`
}

// cpuSample stores raw CPU and wall-clock times for delta calculation.
type cpuSample struct {
	wallTime       time.Time
	processCPUTime int64 // nanoseconds (kernel + user)
	sysBusyTime    int64 // nanoseconds
	sysTotalTime   int64 // nanoseconds
}

// Collector gathers runtime, memory, CPU and storage telemetry.
type Collector struct {
	startedAt time.Time

	mu            sync.Mutex
	lastCPUSample cpuSample
	cachedCPU     CPUMetrics
}

// NewCollector builds a telemetry collector and initializes baseline counters.
func NewCollector() *Collector {
	c := &Collector{
		startedAt: time.Now(),
	}
	c.initBaseline()
	return c
}

// SystemInfo snapshots the server runtime environment.
func (c *Collector) SystemInfo() SystemInfo {
	return SystemInfo{
		UptimeSeconds: int64(time.Since(c.startedAt).Seconds()),
		StartedAt:     c.startedAt.Unix(),
		Goroutines:    runtime.NumGoroutine(),
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		ServerVersion: buildinfo.Version,
	}
}

// goHeapSamples are the two runtime figures this reports, named as
// runtime/metrics knows them.
//
// They are read through that package rather than through runtime.MemStats
// because ReadMemStats stops the world for the length of the call. This server
// carries live audio, and a dashboard polling a metrics endpoint every second
// is not a reason to pause every voice room on the box. The two are the same
// numbers: the runtime documents these as the equivalents of MemStats.Alloc
// and MemStats.Sys.
var goHeapSamples = []metrics.Sample{
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/memory/classes/total:bytes"},
}

// goMemory is the Go runtime's own view of what this process is holding.
func goMemory() (heapAlloc, totalSys int64) {
	samples := make([]metrics.Sample, len(goHeapSamples))
	copy(samples, goHeapSamples)
	metrics.Read(samples)

	// A name the runtime does not know reads back as KindBad, which is what
	// would happen if one of these were ever renamed. Zero is then reported
	// rather than a wrong number, and the RSS below stands in for it.
	if samples[0].Value.Kind() == metrics.KindUint64 {
		heapAlloc = int64(samples[0].Value.Uint64())
	}
	if samples[1].Value.Kind() == metrics.KindUint64 {
		totalSys = int64(samples[1].Value.Uint64())
	}
	return heapAlloc, totalSys
}

// Memory collects process RSS, Go runtime memory, and system physical RAM.
func (c *Collector) Memory() MemoryMetrics {
	heapAlloc, totalSys := goMemory()

	mem := sampleOSMemory()
	mem.ProcessHeapAlloc = heapAlloc
	mem.ProcessHeapSys = totalSys

	if mem.ProcessRSS <= 0 {
		mem.ProcessRSS = totalSys
	}
	if mem.SystemTotal > 0 && mem.SystemUsed > 0 {
		mem.SystemPercent = float64(mem.SystemUsed) / float64(mem.SystemTotal) * 100.0
		if mem.SystemPercent > 100.0 {
			mem.SystemPercent = 100.0
		}
	}
	return mem
}

// Disk returns the total and free space on the drive holding path.
func (c *Collector) Disk(path string) DiskMetrics {
	return sampleOSDisk(path)
}

// CPU calculates the recent CPU utilization of both the Aural process
// and the overall host system since the last sample.
func (c *Collector) CPU() CPUMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	deltaWall := now.Sub(c.lastCPUSample.wallTime)

	// If sampled too quickly (less than 400ms), return cached value to avoid zero-delta noise
	if deltaWall < 400*time.Millisecond && c.cachedCPU.Cores > 0 {
		return c.cachedCPU
	}

	currProcessNs, currSysBusyNs, currSysTotalNs, err := sampleOSCPUTimes()
	if err != nil {
		return CPUMetrics{Cores: runtime.NumCPU()}
	}

	cores := runtime.NumCPU()
	if cores <= 0 {
		cores = 1
	}

	deltaProcess := currProcessNs - c.lastCPUSample.processCPUTime
	deltaWallNs := deltaWall.Nanoseconds()

	var procPercent float64
	if deltaWallNs > 0 && deltaProcess >= 0 {
		procPercent = (float64(deltaProcess) / float64(deltaWallNs*int64(cores))) * 100.0
		if procPercent > 100.0 {
			procPercent = 100.0
		}
		if procPercent < 0.0 {
			procPercent = 0.0
		}
	}

	var sysPercent float64
	deltaSysBusy := currSysBusyNs - c.lastCPUSample.sysBusyTime
	deltaSysTotal := currSysTotalNs - c.lastCPUSample.sysTotalTime
	if deltaSysTotal > 0 && deltaSysBusy >= 0 {
		sysPercent = (float64(deltaSysBusy) / float64(deltaSysTotal)) * 100.0
		if sysPercent > 100.0 {
			sysPercent = 100.0
		}
		if sysPercent < 0.0 {
			sysPercent = 0.0
		}
	} else {
		sysPercent = procPercent
	}

	c.lastCPUSample = cpuSample{
		wallTime:       now,
		processCPUTime: currProcessNs,
		sysBusyTime:    currSysBusyNs,
		sysTotalTime:   currSysTotalNs,
	}

	c.cachedCPU = CPUMetrics{
		ProcessPercent: procPercent,
		SystemPercent:  sysPercent,
		Cores:          cores,
	}
	return c.cachedCPU
}

func (c *Collector) initBaseline() {
	c.mu.Lock()
	defer c.mu.Unlock()

	procNs, sysBusyNs, sysTotalNs, err := sampleOSCPUTimes()
	if err == nil {
		c.lastCPUSample = cpuSample{
			wallTime:       time.Now(),
			processCPUTime: procNs,
			sysBusyTime:    sysBusyNs,
			sysTotalTime:   sysTotalNs,
		}
	}
	c.cachedCPU = CPUMetrics{
		Cores: runtime.NumCPU(),
	}
}
