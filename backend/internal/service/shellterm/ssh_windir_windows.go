//go:build windows

package shellterm

import "golang.org/x/sys/windows"

// systemWindowsDirectory is the Windows folder as the kernel reports it, the
// one ssh is started from (see sshProgramPath).
func systemWindowsDirectory() (string, error) {
	return windows.GetSystemWindowsDirectory()
}
