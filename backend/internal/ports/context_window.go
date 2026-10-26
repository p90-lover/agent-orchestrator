package ports

import (
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ContextWindowUnsupportedReason names only client knobs AO actually transports.
// A supported declaration does not increase the provider's real model capacity.
func ContextWindowUnsupportedReason(harness domain.AgentHarness, model string) string {
	switch harness {
	case domain.HarnessCodex, domain.HarnessClaudeCode:
		return ""
	case domain.HarnessOpenCode:
		provider, id, ok := strings.Cut(model, "/")
		if ok && strings.TrimSpace(provider) != "" && strings.TrimSpace(id) != "" {
			return ""
		}
		return "OpenCode contextWindow requires an explicit provider/model selection"
	default:
		return fmt.Sprintf("%s has no verified client contextWindow override", harness)
	}
}

// ClaudeContextWindowUsesAutoCompact keeps native models on the documented
// compaction budget instead of the custom-model capacity override.
func ClaudeContextWindowUsesAutoCompact(model string) bool {
	id := strings.ToLower(strings.TrimSpace(model))
	base, _, _ := strings.Cut(id, "(")
	switch strings.TrimSpace(base) {
	case "", "default", "best", "opusplan", "sonnet", "opus", "haiku", "fable":
		return true
	}
	return strings.Contains(id, "claude") || strings.Contains(id, "[1m]")
}

// ValidateNativeEffort protects explicit requests for adapters whose full spawn
// path does not carry reasoning effort. Inherited defaults keep their old behavior.
// CPA effort is encoded in its wire model, so native effort remains suppressed.
func ValidateNativeEffort(harness domain.AgentHarness, effort string, gateway bool) error {
	if effort == "" || gateway || harness == domain.HarnessCodex || harness == domain.HarnessClaudeCode {
		return nil
	}
	return fmt.Errorf("%w: %s has no verified native effort spawn path", ErrUnsupportedEffort, harness)
}

// ValidateContextWindow rejects requested tuning rather than silently dropping it.
func ValidateContextWindow(harness domain.AgentHarness, model string, tokens int64) error {
	if tokens < 0 {
		return fmt.Errorf("contextWindow must be positive, or zero to keep the client default")
	}
	if tokens == 0 {
		return nil
	}
	if reason := ContextWindowUnsupportedReason(harness, model); reason != "" {
		return fmt.Errorf("unsupported contextWindow: %s", reason)
	}
	if harness == domain.HarnessClaudeCode && ClaudeContextWindowUsesAutoCompact(model) && (tokens < 100000 || tokens > 1000000) {
		return fmt.Errorf("Claude Code contextWindow auto-compact budget must be between 100000 and 1000000 tokens")
	}
	return nil
}
