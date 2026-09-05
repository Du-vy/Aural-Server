//go:build windows

package system

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetProcessTimes      = kernel32.NewProc("GetProcessTimes")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetCurrentProcess    = kernel32.NewProc("GetCurrentProcess")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

type fileTime struct {
	lowDateTime  uint32
	highDateTime uint32
}

func (ft *fileTime) nanoseconds() int64 {
	return (int64(ft.highDateTime)<<32 + int64(ft.lowDateTime)) * 100
}

func sampleOSMemory() MemoryMetrics {
	var out MemoryMetrics

	// Physical RAM on host
	var mem memoryStatusEx
	mem.length = uint32(unsafe.Sizeof(mem))
	if ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&mem))); ret != 0 {
		out.SystemTotal = int64(mem.totalPhys)
		out.SystemFree = int64(mem.availPhys)
		out.SystemUsed = int64(mem.totalPhys - mem.availPhys)
		out.SystemPercent = float64(mem.memoryLoad)
	}

	// Working Set (physical RAM resident for Aural process)
	hProcess, _, _ := procGetCurrentProcess.Call()
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if ret, _, _ := procGetProcessMemoryInfo.Call(hProcess, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); ret != 0 {
		out.ProcessRSS = int64(pmc.workingSetSize)
	}

	return out
}

func sampleOSDisk(path string) DiskMetrics {
	if path == "" {
		path = "."
	}
	var freeBytes, totalBytes, totalFreeBytes uint64
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return DiskMetrics{}
	}
	ret, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytes)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if ret != 0 {
		return DiskMetrics{
			TotalBytes: int64(totalBytes),
			FreeBytes:  int64(freeBytes),
		}
	}
	return DiskMetrics{}
}

func sampleOSCPUTimes() (processNs, sysBusyNs, sysTotalNs int64, err error) {
	hProcess, _, _ := procGetCurrentProcess.Call()
	var creationTime, exitTime, kernelTime, userTime fileTime
	ret, _, callErr := procGetProcessTimes.Call(
		hProcess,
		uintptr(unsafe.Pointer(&creationTime)),
		uintptr(unsafe.Pointer(&exitTime)),
		uintptr(unsafe.Pointer(&kernelTime)),
		uintptr(unsafe.Pointer(&userTime)),
	)
	if ret == 0 {
		return 0, 0, 0, callErr
	}
	processNs = kernelTime.nanoseconds() + userTime.nanoseconds()

	var idleTime, sysKernelTime, sysUserTime fileTime
	ret, _, callErr = procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleTime)),
		uintptr(unsafe.Pointer(&sysKernelTime)),
		uintptr(unsafe.Pointer(&sysUserTime)),
	)
	if ret == 0 {
		return processNs, 0, 0, callErr
	}

	// In Windows GetSystemTimes, sysKernelTime includes idleTime!
	// Total system time = sysKernelTime + sysUserTime
	// Busy system time = (sysKernelTime + sysUserTime) - idleTime
	idleNs := idleTime.nanoseconds()
	totalNs := sysKernelTime.nanoseconds() + sysUserTime.nanoseconds()
	busyNs := totalNs - idleNs
	if busyNs < 0 {
		busyNs = 0
	}
	return processNs, busyNs, totalNs, nil
}
