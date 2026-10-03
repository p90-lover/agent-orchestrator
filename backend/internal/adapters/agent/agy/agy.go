// Package agy implements the Agy (Antigravity) agent adapter: launching new sessions,
// resuming sessions by native ID, installing workspace-local hooks, and reading
// hook-derived session info.
package agy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/agentbase"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/binaryutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const adapterID = "agy"

var agyBinarySpec = binaryutil.BinarySpec{
	Label:         "agy",
	Names:         []string{"agy"},
	WinNames:      []string{"agy.cmd", "agy.exe", "agy"},
	UnixPaths:     []string{"/usr/local/bin/agy", "/opt/homebrew/bin/agy"},
	UnixHomePaths: binaryutil.NodeManagedUnixHomePaths("agy", []string{".cargo", "bin", "agy"}),
	NodeManaged:   true,
	WinPaths: []binaryutil.WinPath{
		{Base: binaryutil.WinAppData, Parts: []string{"npm", "agy.cmd"}},
		{Base: binaryutil.WinAppData, Parts: []string{"npm", "agy.exe"}},
		{Base: binaryutil.WinHome, Parts: []string{".cargo", "bin", "agy.exe"}},
	},
}

// Plugin is the Agy agent adapter. It is safe for concurrent use; the binary
// path is resolved once and cached under binaryMu.
type Plugin struct {
	agentbase.Base
	binaryMu       sync.RWMutex
	resolvedBinary string
}

// New returns a ready-to-register Agy adapter.
func New() *Plugin {
	return &Plugin{}
}

var _ adapters.Adapter = (*Plugin)(nil)
var _ ports.Agent = (*Plugin)(nil)
var _ ports.SubmitActivitySignaler = (*Plugin)(nil)
var _ ports.BlockedActivitySignaler = (*Plugin)(nil)

// EmitsSubmitActivity reports that PreInvocation proves submitted work has
// reached AGY's execution loop.
func (p *Plugin) EmitsSubmitActivity() bool { return true }

// EmitsBlockedActivity is false because current AGY hooks do not expose a
// permission-wait event that AO can safely correlate with a session.
func (p *Plugin) EmitsBlockedActivity() bool { return false }

// Manifest returns the adapter's static self-description.
func (p *Plugin) Manifest() adapters.Manifest {
	return adapters.Manifest{
		ID:          adapterID,
		Name:        "Agy",
		Description: "Run Agy (Antigravity) worker sessions.",
		Version:     "0.0.1",
		Capabilities: []adapters.Capability{
			adapters.CapabilityAgent,
		},
	}
}

// GetConfigSpec reports the per-project agent config keys the Agy (Antigravity)
// CLI understands: a model override passed via --model. Confirmed via
// `agy --help`: "--model  Model for the current CLI session".
func (p *Plugin) GetConfigSpec(ctx context.Context) (ports.ConfigSpec, error) {
	if err := ctx.Err(); err != nil {
		return ports.ConfigSpec{}, err
	}
	return ports.ConfigSpec{
		Fields: []ports.ConfigField{
			{
				Key:         "model",
				Type:        ports.ConfigFieldString,
				Description: "Model override passed to `agy --model` (e.g. gemini-3-pro).",
			},
		},
	}, nil
}

// GetLaunchCommand builds the argv to start an interactive Agy session.
// Shape:
//
//	agy --add-dir <WorkspacePath> [--dangerously-skip-permissions] [--model <Model>] [--prompt-interactive <Prompt>]
func (p *Plugin) GetLaunchCommand(ctx context.Context, cfg ports.LaunchConfig) (cmd []string, err error) {
	binary, err := p.agyBinary(ctx)
	if err != nil {
		return nil, err
	}

	cmd = []string{binary}

	if cfg.WorkspacePath != "" {
		cmd = append(cmd, "--add-dir", cfg.WorkspacePath)
	}

	if cfg.Permissions == ports.PermissionModeBypassPermissions {
		cmd = append(cmd, "--dangerously-skip-permissions")
	}

	appendModelFlag(&cmd, cfg.Config)

	if cfg.Prompt != "" {
		prompt := cfg.Prompt
		if isBatchLauncher(binary, runtime.GOOS) {
			dir, pointer, err := writeLaunchPromptFile(cfg)
			if err != nil {
				return nil, err
			}
			cmd = append(cmd, "--add-dir", dir)
			prompt = pointer
		}
		cmd = append(cmd, "--prompt-interactive", prompt)
	}

	return cmd, nil
}

// isBatchLauncher reports whether agy starts through a Windows batch file (a
// managed or npm-installed agy.cmd). cmd.exe ends the command line at the first
// newline and expands %, &, | and quotes inside arguments, so a prompt passed
// inline would arrive cut off at its first line, or mangled.
func isBatchLauncher(binary, goos string) bool {
	if goos != "windows" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(binary))
	return ext == ".cmd" || ext == ".bat"
}

// writeLaunchPromptFile stores the initial prompt beside the session's system
// prompt and returns that folder plus a one-line, cmd-safe prompt telling agy
// to read it. The folder is added to agy's workspace so it can read the file.
func writeLaunchPromptFile(cfg ports.LaunchConfig) (dir, pointer string, err error) {
	switch {
	case cfg.SystemPromptFile != "":
		dir = filepath.Dir(cfg.SystemPromptFile)
	case cfg.DataDir != "" && cfg.SessionID != "":
		dir = filepath.Join(cfg.DataDir, "prompts", cfg.SessionID)
	default:
		return "", "", fmt.Errorf("agy: no session folder for the launch prompt")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("agy: prepare launch prompt folder: %w", err)
	}
	file := filepath.Join(dir, "launch-prompt.md")
	if err := os.WriteFile(file, []byte(cfg.Prompt), 0o600); err != nil {
		return "", "", fmt.Errorf("agy: write launch prompt: %w", err)
	}
	return dir, "Your task is in the file " + file + " - read that whole file now and follow it exactly.", nil
}

// GetRestoreCommand rebuilds the argv that continues an existing Agy session:
// `agy --add-dir <WorkspacePath> [--dangerously-skip-permissions] [--model <Model>] --conversation <agentSessionId>`.
func (p *Plugin) GetRestoreCommand(ctx context.Context, cfg ports.RestoreConfig) (cmd []string, ok bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	agentSessionID := strings.TrimSpace(cfg.Session.Metadata[ports.MetadataKeyAgentSessionID])
	if agentSessionID == "" {
		return nil, false, nil
	}

	binary, err := p.agyBinary(ctx)
	if err != nil {
		return nil, false, err
	}

	cmd = []string{binary}

	if cfg.Session.WorkspacePath != "" {
		cmd = append(cmd, "--add-dir", cfg.Session.WorkspacePath)
	}

	if cfg.Permissions == ports.PermissionModeBypassPermissions {
		cmd = append(cmd, "--dangerously-skip-permissions")
	}

	appendModelFlag(&cmd, cfg.Config)

	cmd = append(cmd, "--conversation", agentSessionID)
	return cmd, true, nil
}

// SessionInfo surfaces Agy hook-derived metadata.
func (p *Plugin) SessionInfo(ctx context.Context, session ports.SessionRef) (ports.SessionInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return ports.SessionInfo{}, false, err
	}
	info, ok := agentbase.StandardSessionInfo(session)
	return info, ok, nil
}

// ResolveAgyBinary returns the path to the agy binary on this machine,
// searching PATH then a handful of well-known install locations. It returns a
// wrapped ports.ErrAgentBinaryNotFound when agy is absent.
func ResolveAgyBinary(ctx context.Context) (string, error) {
	return binaryutil.ResolveBinary(ctx, agyBinarySpec)
}

func (p *Plugin) agyBinary(ctx context.Context) (string, error) {
	// Fast path: a concurrent-safe read of the already-resolved binary.
	p.binaryMu.RLock()
	cached := p.resolvedBinary
	p.binaryMu.RUnlock()
	if cached != "" {
		return cached, nil
	}

	// Populate path: take the write lock and re-check, since another goroutine
	// may have resolved the binary between releasing RLock and acquiring Lock.
	p.binaryMu.Lock()
	defer p.binaryMu.Unlock()
	if p.resolvedBinary != "" {
		return p.resolvedBinary, nil
	}

	binary, err := ResolveAgyBinary(ctx)
	if err != nil {
		return "", err
	}
	p.resolvedBinary = binary
	return binary, nil
}

// appendModelFlag appends `--model <trimmed>` when cfg.Model is set,
// mirroring the Codex adapter's pattern (see #2869). A blank or
// whitespace-only value is omitted so agy falls back to its own default
// model resolution exactly as an unconfigured launch would. Confirmed via
// `agy --help`: "--model  Model for the current CLI session".
func appendModelFlag(cmd *[]string, cfg ports.AgentConfig) {
	if model := strings.TrimSpace(cfg.Model); model != "" {
		*cmd = append(*cmd, "--model", model)
	}
}
