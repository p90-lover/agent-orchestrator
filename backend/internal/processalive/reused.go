package processalive

import "time"

// ReusedSince reports whether pid now belongs to a process that started after
// startedBy: a different program from the one that was recorded with that time.
// A caller holding only a process id and the time it recorded it uses this to
// avoid treating that other program as its own, for example before killing it.
//
// It is false when pid maps to no process, when startedBy is zero, and on
// systems where AliveSince does not check start times.
func ReusedSince(pid int, startedBy time.Time) bool {
	return !startedBy.IsZero() && Alive(pid) && !AliveSince(pid, startedBy)
}
