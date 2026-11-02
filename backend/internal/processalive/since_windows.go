//go:build windows

package processalive

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// AliveSince reports whether pid exists and its process started no later than
// startedBy. Windows hands a dead process's id to the next process soon: a chat
// host that died during an app update had its id taken by an unrelated bun.exe
// six minutes later (2026-10-31), and Alive kept calling the host live, so AO
// could neither reattach nor stop it. A process created after startedBy is that
// other program. A zero startedBy, or a process this user may not query, falls
// back to Alive.
func AliveSince(pid int, startedBy time.Time) bool {
	if pid <= 0 {
		return false
	}
	if startedBy.IsZero() {
		return Alive(pid)
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return Alive(pid)
		}
		return false
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return true
	}
	return !time.Unix(0, creation.Nanoseconds()).After(startedBy.Add(startMargin))
}

// startMargin keeps a live owner live even if the clock stepped back between
// its process starting and it recording startedBy. A reusing process starts
// only after the owner died, so the margin costs nothing but a host that died
// within it.
const startMargin = 2 * time.Second
