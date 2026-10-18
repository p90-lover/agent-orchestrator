//go:build windows

package persistenthost

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// hostCreationFlags start the chat host with a console of its own that has no
// window (CREATE_NO_WINDOW), never DETACHED_PROCESS. A detached host has no
// console at all, and the agent it starts, the Git Bash its Bash tool runs and
// every process that Bash forks then come up without the console context
// MSYS2/Cygwin initialises in each forked child: those forks failed with
// "dofork: child -1 - CreateProcess failed ... 0xC0000142" and "fork: Resource
// temporarily unavailable". Its own console (not the daemon's) still lets the
// host outlive the daemon, and its own process group keeps the daemon's
// Ctrl+C away from it.
const hostCreationFlags = windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.ABOVE_NORMAL_PRIORITY_CLASS

func spawnDetached(ctx context.Context, cfg Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, hostArgs(cfg)...)
	cmd.Dir = cfg.Workdir
	cmd.Env = cfg.Env
	// Above normal, like the agent it hosts: the host relays every ACP message, so
	// a host starved of CPU slows the handshake as much as a starved agent.
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: hostCreationFlags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached chat host: %w", err)
	}
	return cmd.Process.Release()
}
