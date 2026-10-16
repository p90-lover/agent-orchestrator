//go:build windows

package persistenthost

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// configureProviderProcess starts the agent above normal priority, as the
// direct ACP launch does (acp.configureProcessGroup): Windows does not pass a
// raised priority on to a child, so the host's own class does not cover it.
func configureProviderProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.ABOVE_NORMAL_PRIORITY_CLASS,
		HideWindow:    true,
	}
}

func killProviderProcess(ctx context.Context, cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	kill.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
