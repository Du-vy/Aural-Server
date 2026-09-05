package system

import (
	"runtime"
	"testing"
	"time"
)

func TestCollector(t *testing.T) {
	c := NewCollector()

	info := c.SystemInfo()
	if info.OS == "" || info.Arch == "" || info.Goroutines <= 0 {
		t.Errorf("unexpected system info: %+v", info)
	}

	mem := c.Memory()
	if mem.ProcessHeapAlloc <= 0 || mem.ProcessHeapSys <= 0 {
		t.Errorf("unexpected memory metrics: %+v", mem)
	}

	disk := c.Disk(".")
	if disk.TotalBytes < 0 || disk.FreeBytes < 0 {
		t.Errorf("unexpected disk metrics: %+v", disk)
	}

	time.Sleep(10 * time.Millisecond)
	cpu := c.CPU()
	if cpu.Cores <= 0 {
		t.Errorf("expected cpu cores > 0, got %+v", cpu)
	}
}

// TestGoMemoryMatchesMemStats pins the two runtime/metrics names this reads
// through instead of runtime.ReadMemStats.
//
// ReadMemStats stops the world, which a server carrying live audio should not
// do because somebody opened a dashboard. The names are the documented
// equivalents of MemStats.Alloc and MemStats.Sys, but they are strings: a
// rename would silently start reporting zero, and the point of the swap is
// that the numbers stay the same.
func TestGoMemoryMatchesMemStats(t *testing.T) {
	// Something on the heap, so neither figure can pass by being empty.
	ballast := make([][]byte, 64)
	for i := range ballast {
		ballast[i] = make([]byte, 64*1024)
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapAlloc, totalSys := goMemory()

	if heapAlloc <= 0 || totalSys <= 0 {
		t.Fatalf("goMemory() = (%d, %d); a renamed metric reads back as zero", heapAlloc, totalSys)
	}
	if heapAlloc > totalSys {
		t.Errorf("heap %d is larger than everything mapped %d", heapAlloc, totalSys)
	}

	// The two are read a moment apart while the program is still running, so
	// they agree in size rather than exactly. An order of magnitude apart
	// would mean the name is pointing at some other number entirely.
	within := func(got, want int64) bool {
		return got > want/4 && got < want*4
	}
	if !within(heapAlloc, int64(ms.Alloc)) {
		t.Errorf("heap objects = %d, but MemStats.Alloc = %d", heapAlloc, ms.Alloc)
	}
	if !within(totalSys, int64(ms.Sys)) {
		t.Errorf("total mapped = %d, but MemStats.Sys = %d", totalSys, ms.Sys)
	}

	runtime.KeepAlive(ballast)
}
