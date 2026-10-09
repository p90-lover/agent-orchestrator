package ports

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestContextWindowCapabilityCoversEveryHarness(t *testing.T) {
	for _, harness := range domain.AllHarnesses {
		model := "openai/gpt-5.5(high)"
		if harness == domain.HarnessClaudeCode {
			model = "gpt-5.5(high)"
		}
		want := harness == domain.HarnessCodex || harness == domain.HarnessClaudeCode || harness == domain.HarnessOpenCode
		if got := ContextWindowUnsupportedReason(harness, model) == ""; got != want {
			t.Errorf("%s context support=%v want=%v", harness, got, want)
		}
		if err := ValidateContextWindow(harness, model, 32768); (err == nil) != want {
			t.Errorf("%s context validation=%v support=%v", harness, err, want)
		}
		nativeEffort := harness == domain.HarnessCodex || harness == domain.HarnessClaudeCode
		if err := ValidateNativeEffort(harness, "high", false); (err == nil) != nativeEffort {
			t.Errorf("%s explicit native effort accepted=%v want=%v", harness, err == nil, nativeEffort)
		}
		if err := ValidateNativeEffort(harness, "", false); err != nil {
			t.Errorf("%s native default rejected: %v", harness, err)
		}
		if err := ValidateContextWindow(harness, model, 0); err != nil {
			t.Errorf("%s default rejected: %v", harness, err)
		}
	}
}

func TestContextWindowRejectsUnsafeOrUnspecifiedModelOverrides(t *testing.T) {
	for _, model := range []string{"", "opus", "sonnet(high)", "claude-sonnet-4-5", "anthropic/claude-opus-4-8", "custom[1m]"} {
		if err := ValidateContextWindow(domain.HarnessClaudeCode, model, 32768); err == nil {
			t.Errorf("Claude context override accepted unsafe model %q", model)
		}
	}
	if err := ValidateContextWindow(domain.HarnessOpenCode, "gpt-5.5", 32768); err == nil {
		t.Fatal("OpenCode accepted a model without provider")
	}
	if err := ValidateContextWindow(domain.HarnessCodex, "gpt-5.5", -1); err == nil {
		t.Fatal("negative context window accepted")
	}
}
