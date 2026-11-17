//go:build unix

package startupprogress

import (
	"runtime"
	"syscall"
	"time"
)

// Self is this process's work so far from getrusage: CPU time everywhere, and
// on Linux the bytes it read from and wrote to block devices (counted in
// 512-byte units there; other systems count operations, so they report none).
func Self() Work {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return Work{}
	}
	work := Work{CPU: time.Duration(usage.Utime.Nano() + usage.Stime.Nano())}
	if runtime.GOOS == "linux" {
		work.IOBytes = uint64(int64(usage.Inblock)+int64(usage.Oublock)) * 512
		work.HasIO = true
	}
	return work
}
