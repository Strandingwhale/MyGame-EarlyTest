//go:build !windows

// 非 Windows 平台：进程采样不可用，全部返回空值（压测功能本身不受影响）
package main

func openProc(pid uint32) uintptr                    { return 0 }
func procTimes(handle uintptr) (uint64, uint64, bool) { return 0, 0, false }
func procWorkingSet(handle uintptr) (uint64, bool)    { return 0, false }
