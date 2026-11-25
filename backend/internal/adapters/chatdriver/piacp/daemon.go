package piacp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const daemonStartupTimeout = 30 * time.Second

// daemonApprovalsMarker, beside the session's socket folder, records that AO
// started the session's pi-acp daemon with the tool-approval env
// (APPROVALS.md). A daemon keeps the env and extension it started with, so one
// running without the marker predates Pi's approvals and would never ask.
const daemonApprovalsMarker = "daemon-approvals"

func startDaemon(ctx context.Context, binary, workspace string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, daemonStartupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	dial := net.Dialer{Timeout: 250 * time.Millisecond}
	socket := filepath.Join(env["PI_ACP_SOCKET_DIR"], "pi-acp-control.sock")
	marker := filepath.Join(filepath.Dir(env["PI_ACP_SOCKET_DIR"]), daemonApprovalsMarker)
	ready := func() bool {
		conn, err := dial.DialContext(ctx, "unix", socket)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	if ready() {
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		// Pi keeps its sessions on disk, and AO resumes this one on the new daemon.
		if err := stopDaemon(ctx, socket, ready); err != nil {
			return fmt.Errorf("stop the Pi ACP daemon started before chat approvals: %w", err)
		}
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
		return err
	}
	if env[envApprovalToken] != "" {
		_ = os.WriteFile(marker, []byte("1\n"), 0o600)
	}
	return nil
}

// stopDaemon asks the daemon on its control socket to shut down, as
// `pi-acp --daemon-stop` does, and waits for the socket to close.
func stopDaemon(ctx context.Context, socket string, ready func() bool) error {
	client := http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dial net.Dialer
			return dial.DialContext(ctx, "unix", socket)
		},
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://daemon/shutdown", http.NoBody)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("shutdown answered HTTP %d", response.StatusCode)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
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
