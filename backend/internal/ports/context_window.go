package ports

import (
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PiGatewayProvider is the Pi provider AO's managed Pi extension registers for a session
// whose model runs through Coding Tools' CPA gateway; Pi runs it as coding-tools-cpa/<model>.
const PiGatewayProvider = "coding-tools-cpa"

// ContextWindowUnsupportedReason names only client knobs AO actually transports.
// A supported declaration does not increase the provider's real model capacity.
func ContextWindowUnsupportedReason(harness domain.AgentHarness, model string) string {
	switch harness {
	case domain.HarnessCodex:
		return ""
	case domain.HarnessPi:
		// Only a CPA gateway model: AO's Pi extension registers it with the session's window.
		if provider, id, ok := strings.Cut(model, "/"); ok && provider == PiGatewayProvider && strings.TrimSpace(id) != "" {
			return ""
		}
		return "Pi takes a contextWindow only for a Coding Tools CPA gateway model"
	case domain.HarnessOpenCode:
		provider, id, ok := strings.Cut(model, "/")
		if ok && strings.TrimSpace(provider) != "" && strings.TrimSpace(id) != "" {
			return ""
		}
		return "OpenCode contextWindow requires an explicit provider/model selection"
	case domain.HarnessClaudeCode:
		// Every model: a custom ID (a CPA alias such as "Claude Opus 5.5") takes its window from
		// CLAUDE_CODE_MAX_CONTEXT_TOKENS, and a recognized Claude model or alias compacts at
		// CLAUDE_CODE_AUTO_COMPACT_WINDOW, capped at its own window (claudecode.setContextWindowEnv).
		return ""
	default:
		return fmt.Sprintf("%s has no verified client contextWindow override", harness)
	}
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
	return nil
}
