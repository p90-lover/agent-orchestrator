//go:build windows

package acp

import (
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// configureProcessGroup starts the chat agent above normal priority. On a
// machine busy with other work its startup otherwise queued behind everything
// else, and the ACP handshake ran past its deadline ("context deadline
// exceeded") before the agent answered. Above-normal, never high: the agent
// gets the CPU first without starving the rest of the machine.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.ABOVE_NORMAL_PRIORITY_CLASS,
		HideWindow:    true,
	}
}

func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Node launches the provider as a child. taskkill /T is the Windows equivalent
	// of killing the Unix process group and avoids leaving Claude behind.
	kill := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	kill.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
