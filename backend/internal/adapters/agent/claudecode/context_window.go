package claudecode

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ApplyContextWindowEnv sets Claude Code's two window knobs for the session.
// It never disables compaction or changes the user's global settings.
func ApplyContextWindowEnv(ctx context.Context, binary, model string, tokens int64, env map[string]string) error {
	if err := ports.ValidateContextWindow(domain.HarnessClaudeCode, model, tokens); err != nil {
		return err
	}
	if tokens == 0 {
		return nil
	}
	version, err := claudeVersion(ctx, binary)
	if err != nil {
		return fmt.Errorf("Claude Code contextWindow requires a verified version 2.1.193 or newer: %w", err)
	}
	return setContextWindowEnv(model, tokens, version, env)
}

// versionCache keeps each Claude binary's `--version` answer, keyed by its path,
// size and modification time. Running it cost every launch that set a window up
// to its 3-second timeout (more on a busy machine); an update rewrites the
// binary, which changes the key, so a stale answer is never used.
var versionCache sync.Map // versionKey -> string

type versionKey struct {
	path    string
	size    int64
	modTime int64
}

func claudeVersion(ctx context.Context, binary string) (string, error) {
	var key versionKey
	cacheable := false
	if info, err := os.Stat(binary); err == nil && !info.IsDir() {
		key = versionKey{path: binary, size: info.Size(), modTime: info.ModTime().UnixNano()}
		if cached, ok := versionCache.Load(key); ok {
			return cached.(string), nil
		}
		cacheable = true
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := aoprocess.CommandContext(probeCtx, binary, "--version").Output()
	if err != nil {
		return "", err
	}
	if cacheable {
		versionCache.Store(key, string(out))
	}
	return string(out), nil
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
	// Claude Code reads the window one of two ways, by whether it recognizes the model. A custom ID
	// (anything but a canonical "claude-…" ID, so a CPA alias like "Claude Opus 5.5" too) has the
	// window CLAUDE_CODE_MAX_CONTEXT_TOKENS declares. A recognized model keeps its own window and
	// ignores that variable; it compacts at the smaller of its window and
	// CLAUDE_CODE_AUTO_COMPACT_WINDOW. Both are set, so the chosen window holds either way, and a
	// recognized model is never taken past its real window.
	value := strconv.FormatInt(tokens, 10)
	env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = value
	env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] = value
	return nil
}
