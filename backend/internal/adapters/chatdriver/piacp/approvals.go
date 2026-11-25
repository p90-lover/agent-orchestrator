package piacp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The pi-acp daemon's env for chat approvals, read by AO's managed Pi extension
// and by `ao hooks pi tool-call` (APPROVALS.md).
const (
	envApprovalSessionID = "AO_PI_APPROVAL_SESSION_ID"
	envApprovalToken     = "AO_PI_APPROVAL_TOKEN"
	envApprovalAOBinary  = "AO_PI_APPROVAL_AO_BIN"
	approvalTokenFile    = "approval-token"
)

// executable is the AO binary the extension runs for `ao hooks pi tool-call`:
// this one, by path, so the hook never depends on PATH.
var executable = os.Executable

// prepareToolApprovals gives the pi-acp daemon what the managed extension needs
// to put a tool call to the chat: the session, its token and AO's executable.
func prepareToolApprovals(cfg acpdriver.LaunchConfig, env map[string]string) error {
	token, err := toolApprovalToken(cfg.DataDir, cfg.SessionID)
	if err != nil {
		return err
	}
	env[envApprovalSessionID] = string(cfg.SessionID)
	env[envApprovalToken] = token
	if path, err := executable(); err == nil && filepath.IsAbs(path) {
		env[envApprovalAOBinary] = path
	}
	return nil
}

// toolApprovalToken is the session's secret for tool approvals: created once,
// then read back, so a pi-acp daemon that outlives an AO restart (it keeps the
// env it started with) still matches the conversation AO opens to it.
func toolApprovalToken(dataDir string, session domain.SessionID) (string, error) {
	if !filepath.IsAbs(dataDir) || strings.TrimSpace(string(session)) == "" {
		return "", fmt.Errorf("pi-acp tool approvals need an absolute data directory and a session id")
	}
	dir := filepath.Join(dataDir, "pi-acp", string(session))
	path := filepath.Join(dir, approvalTokenFile)
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // AO-owned path under its data dir
		if token := strings.TrimSpace(string(data)); len(token) == 64 {
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read the Pi approval token: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("make the Pi approval token: %w", err)
	}
	token := hex.EncodeToString(secret)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("prepare the Pi approval token: %w", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write the Pi approval token: %w", err)
	}
	return token, nil
}
