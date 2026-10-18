//go:build windows

package persistenthost

import (
	"testing"

	"golang.org/x/sys/windows"
)

// A DETACHED_PROCESS chat host has no console, and Git Bash forks run by the
// agent it hosts failed with 0xC0000142. The host gets a windowless console of
// its own instead.
func TestHostCreationFlagsGiveTheHostAWindowlessConsole(t *testing.T) {
	if hostCreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Fatalf("hostCreationFlags = %#x: DETACHED_PROCESS leaves the host without a console", hostCreationFlags)
	}
	for name, flag := range map[string]uint32{
		"CREATE_NO_WINDOW":         windows.CREATE_NO_WINDOW,
		"CREATE_NEW_PROCESS_GROUP": windows.CREATE_NEW_PROCESS_GROUP,
	} {
		if hostCreationFlags&flag == 0 {
			t.Fatalf("hostCreationFlags = %#x, want %s", hostCreationFlags, name)
		}
	}
}
