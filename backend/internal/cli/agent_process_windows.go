//go:build windows

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// supervisedCommand starts an agent's argv. Windows runs a batch launcher (an
// npm or Coding Tools agy.cmd) through cmd.exe, which drops the quotes around a
// launcher path with spaces as soon as any argument is quoted too, so the agent
// never starts ("'C:\...\Coding' is not recognized"). A batch launcher gets an
// explicit `cmd.exe /d /s /c "<quoted argv>"` line instead: /s removes only
// the outer pair of quotes and keeps every inner one.
func supervisedCommand(ctx context.Context, argv []string) *exec.Cmd {
	ext := strings.ToLower(filepath.Ext(argv[0]))
	if ext != ".cmd" && ext != ".bat" {
		return exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is constructed by the selected agent adapter.
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	child := exec.CommandContext(ctx, comspec) //nolint:gosec // the batch argv comes from the selected agent adapter.
	child.SysProcAttr = &syscall.SysProcAttr{CmdLine: batchCommandLine(comspec, argv)}
	return child
}

func batchCommandLine(comspec string, argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = syscall.EscapeArg(arg)
	}
	return syscall.EscapeArg(comspec) + ` /d /s /c "` + strings.Join(quoted, " ") + `"`
}
