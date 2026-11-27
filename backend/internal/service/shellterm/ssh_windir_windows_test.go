//go:build windows

package shellterm

import (
	"testing"
)

// The kernel's Windows folder is a plain drive path the program rule takes.
func TestSystemWindowsDirectoryIsAPlainDrivePath(t *testing.T) {
	dir, err := systemWindowsDirectory()
	if err != nil {
		t.Fatalf("systemWindowsDirectory: %v", err)
	}
	program, ok := sshProgramPath(true, systemWindowsDirectory)
	if !ok || !sshDriveRootPattern.MatchString(program) {
		t.Fatalf("program from %q = %q, %v; want a drive path", dir, program, ok)
	}
}
