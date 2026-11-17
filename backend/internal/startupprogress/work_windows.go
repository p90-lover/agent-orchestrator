//go:build windows

package startupprogress

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

// Self is this process's work so far: CPU time from GetProcessTimes, and the
// bytes of every read and write (files, pipes, devices) from
// GetProcessIoCounters. A query that fails leaves its part zero.
func Self() Work {
	var work Work
	process := windows.CurrentProcess()
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(process, &creation, &exit, &kernel, &user) == nil {
		// A FILETIME duration counts 100 ns ticks.
		ticks := (int64(kernel.HighDateTime)<<32 | int64(kernel.LowDateTime)) +
			(int64(user.HighDateTime)<<32 | int64(user.LowDateTime))
		work.CPU = time.Duration(ticks) * 100
	}
	var counters windows.IO_COUNTERS
	if ok, _, _ := procGetProcessIoCounters.Call(uintptr(process), uintptr(unsafe.Pointer(&counters))); ok != 0 {
		work.IOBytes = counters.ReadTransferCount + counters.WriteTransferCount
		work.HasIO = true
	}
	return work
}
