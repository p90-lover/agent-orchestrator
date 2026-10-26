package claudecode

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// ApplyContextWindowEnv sets Claude's documented session-local context knob.
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
		return fmt.Errorf("cannot verify Claude Code contextWindow version: %w", err)
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
	windowEnv, inactiveWindowEnv := "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	minimumPatch := 193
	autoCompactBudget := ports.ClaudeContextWindowUsesAutoCompact(model)
	if autoCompactBudget {
		windowEnv, inactiveWindowEnv = inactiveWindowEnv, windowEnv
		minimumPatch = 304
	}
	var major, minor, patch int
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return fmt.Errorf("Claude Code contextWindow version is unavailable")
	}
	if _, err := fmt.Sscanf(fields[0], "%d.%d.%d", &major, &minor, &patch); err != nil ||
		major < 2 || (major == 2 && (minor < 1 || (minor == 1 && patch < minimumPatch))) {
		return fmt.Errorf("Claude Code contextWindow via %s requires version 2.1.%d or newer", windowEnv, minimumPatch)
	}
	if env == nil {
		return fmt.Errorf("Claude Code contextWindow requires a session-local launch environment")
	}
	if autoCompactBudget {
		for _, entry := range processenv.Merge(env) {
			key, value, _ := strings.Cut(entry, "=")
			if runtime.GOOS == "windows" {
				key = strings.ToUpper(key)
			}
			if key != "DISABLE_COMPACT" && key != "DISABLE_AUTO_COMPACT" {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "1", "true", "yes", "on":
				return fmt.Errorf("Claude Code contextWindow requires automatic compaction; %s is enabled (choose Auto or remove that override)", key)
			}
		}
	}
	for key := range env {
		if strings.EqualFold(key, windowEnv) || strings.EqualFold(key, inactiveWindowEnv) {
			delete(env, key)
		}
	}
	env[windowEnv] = strconv.FormatInt(tokens, 10)
	// An empty override also masks a competing value inherited from the daemon.
	env[inactiveWindowEnv] = ""
	return nil
}
