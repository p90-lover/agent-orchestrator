package piacp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const daemonStartupTimeout = 30 * time.Second

func startDaemon(ctx context.Context, binary, workspace string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, daemonStartupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dial := net.Dialer{Timeout: 250 * time.Millisecond}
	socket := filepath.Join(env["PI_ACP_SOCKET_DIR"], "pi-acp-control.sock")
	ready := func() bool {
		conn, err := dial.DialContext(ctx, "unix", socket)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	if ready() {
		return nil
	}

	// Start the installed adapter directly; its stdio client's own cold-start deadline is three seconds.
	cmd := aoprocess.Command(binary, "--daemon")
	cmd.Env = processenv.Merge(env)
	cmd.Dir = workspace
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	err := waitForDaemon(ctx, ready, exited)
	if err != nil {
		_ = cmd.Process.Kill()
	}
	return err
}

func waitForDaemon(ctx context.Context, ready func() bool, exited <-chan error) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			if ready() {
				return nil
			}
			if err == nil {
				return errors.New("Pi ACP exited before its private socket was ready")
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return fmt.Errorf("Pi ACP exited before its private socket was ready (exit %d)", exit.ExitCode())
			}
			return err
		case <-ticker.C:
		}
	}
}
