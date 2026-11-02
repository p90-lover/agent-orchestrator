//go:build windows

package cli

import (
	"context"
	"os/exec"
	"syscall"

	"github.com/aoagents/agent-orchestrator/backend/internal/winbatch"
)

// supervisedCommand starts an agent's argv. A batch launcher goes through an
// explicit cmd.exe line (see package winbatch); executables start directly.
func supervisedCommand(ctx context.Context, argv []string) *exec.Cmd {
	if !winbatch.IsLauncher(argv[0]) {
		return exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is constructed by the selected agent adapter.
	}
	comspec := winbatch.Comspec()
	child := exec.CommandContext(ctx, comspec) //nolint:gosec // the batch argv comes from the selected agent adapter.
	child.SysProcAttr = &syscall.SysProcAttr{CmdLine: winbatch.CommandLine(comspec, argv)}
	return child
}
