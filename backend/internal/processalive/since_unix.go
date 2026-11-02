//go:build !windows

package processalive

import "time"

// AliveSince reports whether pid maps to a running process. Unlike Windows,
// these systems hand out ids in sequence and reuse one only after wrapping
// around, so the start time is not checked here.
func AliveSince(pid int, _ time.Time) bool {
	return Alive(pid)
}
