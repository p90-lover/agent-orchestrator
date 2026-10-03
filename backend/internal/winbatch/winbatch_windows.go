//go:build windows

// Package winbatch starts Windows batch launchers (an npm or Coding Tools
// agy.cmd) safely. Windows runs a .cmd/.bat through cmd.exe, which drops the
// quotes around a launcher path with spaces as soon as any argument is quoted
// too, so the launcher never starts ("'C:\...\Coding' is not recognized").
// The fix is an explicit `cmd.exe /d /s /c "<quoted argv>"` line: /s removes
// only the outer pair of quotes and keeps every inner one.
package winbatch

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// IsLauncher reports whether path is a batch file that Windows would run
// through cmd.exe.
func IsLauncher(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}

// Comspec is the command interpreter to run batch launchers with.
func Comspec() string {
	if comspec := os.Getenv("ComSpec"); comspec != "" {
		return comspec
	}
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
}

// CommandLine is the full CreateProcess command line that runs argv (a batch
// launcher and its arguments) through comspec.
func CommandLine(comspec string, argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = syscall.EscapeArg(arg)
	}
	return syscall.EscapeArg(comspec) + ` /d /s /c "` + strings.Join(quoted, " ") + `"`
}
