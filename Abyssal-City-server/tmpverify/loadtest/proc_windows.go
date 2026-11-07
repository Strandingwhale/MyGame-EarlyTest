//go:build windows

// Windows 进程资源采样：OpenProcess + GetProcessTimes（CPU）+ GetProcessMemoryInfo（内存）。
// 无需外部依赖，直接走 syscall；SIZE_T 字段用 uintptr 声明，32/64 位构建都正确。
package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	psapi                  = syscall.NewLazyDLL("psapi.dll")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procGetProcessTimes    = kernel32.NewProc("GetProcessTimes")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

const processQueryInfoVMRead = 0x0400 | 0x0010 // PROCESS_QUERY_INFORMATION | PROCESS_VM_READ

// openProc 按 pid 打开进程句柄，失败返回 0
func openProc(pid uint32) uintptr {
	h, _, _ := procOpenProcess.Call(processQueryInfoVMRead, 0, uintptr(pid))
	return h
}

type fileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (f fileTime) u64() uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// procTimes 返回进程的累计内核态/用户态 CPU 时间（100ns 单位）
func procTimes(handle uintptr) (kernel, user uint64, ok bool) {
	var create, exit, k, u fileTime
	r, _, _ := procGetProcessTimes.Call(handle,
		uintptr(unsafe.Pointer(&create)), uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, false
	}
	return k.u64(), u.u64(), true
}

// processMemoryCounters 对应 Win32 PROCESS_MEMORY_COUNTERS；SIZE_T 用 uintptr 对齐
type processMemoryCounters struct {
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

// procWorkingSet 返回进程当前工作集内存（字节）
func procWorkingSet(handle uintptr) (uint64, bool) {
	var mc processMemoryCounters
	mc.cb = uint32(unsafe.Sizeof(mc))
	r, _, _ := procGetProcessMemoryInfo.Call(handle, uintptr(unsafe.Pointer(&mc)), uintptr(mc.cb))
	if r == 0 {
		return 0, false
	}
	return uint64(mc.WorkingSetSize), true
}
