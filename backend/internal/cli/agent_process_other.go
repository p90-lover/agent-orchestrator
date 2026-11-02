//go:build !windows

package cli

import (
	"context"
	"os/exec"
)

// supervisedCommand starts an agent's argv as is; see the Windows variant for
// batch launchers.
func supervisedCommand(ctx context.Context, argv []string) *exec.Cmd {
	return exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is constructed by the selected agent adapter.
}
