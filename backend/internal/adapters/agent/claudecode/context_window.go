package claudecode

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ApplyContextWindowEnv sets only Claude's documented custom-model window knob.
// It never disables compaction or changes the user's global settings.
func ApplyContextWindowEnv(ctx context.Context, binary, model string, tokens int64, env map[string]string) error {
	if err := ports.ValidateContextWindow(domain.HarnessClaudeCode, model, tokens); err != nil {
		return err
	}
	if tokens == 0 {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := aoprocess.CommandContext(probeCtx, binary, "--version").Output()
	if err != nil {
		return fmt.Errorf("Claude Code contextWindow requires a verified version 2.1.193 or newer: %w", err)
	}
	return setContextWindowEnv(model, tokens, string(out), env)
}

func setContextWindowEnv(model string, tokens int64, version string, env map[string]string) error {
	if err := ports.ValidateContextWindow(domain.HarnessClaudeCode, model, tokens); err != nil {
		return err
	}
	if tokens == 0 {
		return nil
	}
	var major, minor, patch int
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return fmt.Errorf("Claude Code contextWindow version is unavailable")
	}
	if _, err := fmt.Sscanf(fields[0], "%d.%d.%d", &major, &minor, &patch); err != nil ||
		major < 2 || (major == 2 && (minor < 1 || (minor == 1 && patch < 193))) {
		return fmt.Errorf("Claude Code contextWindow requires version 2.1.193 or newer")
	}
	if env == nil {
		return fmt.Errorf("Claude Code contextWindow requires a session-local launch environment")
	}
	env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = strconv.FormatInt(tokens, 10)
	return nil
}
