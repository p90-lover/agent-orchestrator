//go:build windows

package persistenthost

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

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
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.ABOVE_NORMAL_PRIORITY_CLASS, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn detached chat host: %w", err)
	}
	return cmd.Process.Release()
}
