package sessionmanager

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSessionGatewayValidation(t *testing.T) {
	valid := []ports.SessionGateway{{}, {Provider: "cpa", Model: "luna"}, {Provider: "cpa", Model: "gemini-3.8-flash-high"}}
	for _, gateway := range valid {
		if err := ValidateSessionGateway(gateway); err != nil {
			t.Fatalf("ValidateSessionGateway(%+v) = %v", gateway, err)
		}
	}
	invalid := []ports.SessionGateway{
		{Provider: "openai", Model: "gpt"},
		{Provider: "cpa", Model: ""},
		{Provider: "cpa", Model: " luna"},
		{Provider: "cpa", Model: "luna\nANTHROPIC_BASE_URL=http://evil"},
	}
	for _, gateway := range invalid {
		if ValidateSessionGateway(gateway) == nil {
			t.Fatalf("ValidateSessionGateway(%+v) accepted an invalid gateway", gateway)
		}
	}
}

func TestSessionGatewaySurvivesRestartAndPointsAgentsAtCPA(t *testing.T) {
	dataDir := t.TempDir()
	first := &Manager{dataDir: dataDir, logger: slog.Default()}
	if err := first.gateways.set(dataDir, "ses-1", ports.SessionGateway{Provider: "cpa", Model: "luna"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dataDir, gatewayFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("gateway file is empty")
	}

	t.Setenv(EnvCodingToolsCPAKey, "cpa-local-key")
	restarted := &Manager{dataDir: dataDir, logger: slog.Default()}
	env := map[string]string{EnvCodingToolsCPAKey: "cpa-local-key", "ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
	restarted.applySessionGateway(env, "ses-1", false)
	want := map[string]string{
		"ANTHROPIC_BASE_URL":   "http://127.0.0.1:8317",
		"ANTHROPIC_AUTH_TOKEN": "cpa-local-key",
		"ANTHROPIC_MODEL":      "luna",
		"OPENAI_BASE_URL":      "http://127.0.0.1:8317/v1",
		EnvCodingToolsCPAKey:   "",
		// agy's gateway mode (no Google sign-in) with its required helper model.
		"AGY_LLM_GATEWAY_URL":     "http://127.0.0.1:8317",
		"AGY_LLM_GATEWAY_API_KEY": "cpa-local-key",
		"AGY_LLM_GATEWAY_MODELS":  "luna,gemini-3.1-flash-lite-preview",
		// Codex runs from a home whose only setting is the CPA provider.
		"CODEX_HOME": filepath.Join(dataDir, "gateway-homes", "codex"),
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("env[%s] = %q, want %q", key, env[key], value)
		}
	}
	config, err := os.ReadFile(filepath.Join(dataDir, "gateway-homes", "codex", "config.toml"))
	if err != nil || !strings.Contains(string(config), `model_provider = "coding_tools_cpa"`) ||
		!strings.Contains(string(config), `env_key = "OPENAI_API_KEY"`) || strings.Contains(string(config), "cpa-local-key") {
		t.Fatalf("Codex gateway config = %q, %v; want the CPA provider and no key", config, err)
	}

	// A session without a gateway keeps its own provider and still never sees the key.
	plain := map[string]string{EnvCodingToolsCPAKey: "cpa-local-key"}
	restarted.applySessionGateway(plain, "ses-2", false)
	if plain[EnvCodingToolsCPAKey] != "" || plain["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("a session without a gateway got gateway variables: %v", plain)
	}
	raw, _ := os.ReadFile(filepath.Join(dataDir, gatewayFileName))
	if string(raw) == "" || strings.Contains(string(raw), "cpa-local-key") {
		t.Fatal("the gateway file must not contain the key")
	}
}

// A CPA gateway model on Claude Code is served by the gateway, so Claude's own model catalog
// (stale here, as it is without AO's ACP runtime) must not reject it.
func TestGatewayModelSkipsTheAgentsOwnModelCatalog(t *testing.T) {
	calls := 0
	m := &Manager{modelCatalog: tuningCatalog{calls: &calls, catalog: ports.AgentModelCatalog{Stale: true,
		Models: []ports.AgentModelInfo{{ID: "sonnet", Efforts: []string{"high"}}}}}}
	project := domain.ProjectConfig{Worker: domain.RoleOverride{AgentConfig: domain.AgentConfig{Effort: "high"}}}
	gateway := ports.SpawnConfig{
		ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		AgentConfig: ports.AgentConfig{Model: "gemini-3.8-flash-high"},
		Gateway:     ports.SessionGateway{Provider: GatewayProviderCPA, Model: "gemini-3.8-flash-high"},
	}
	resolved, err := m.resolveAgentConfig(context.Background(), gateway, project)
	if err != nil || resolved.Model != "gemini-3.8-flash-high" || resolved.Effort != "" || calls != 0 {
		t.Fatalf("gateway model = %#v, %v (catalog calls %d)", resolved, err, calls)
	}
	// Without the gateway the same model is still checked against Claude's catalog.
	gateway.Gateway = ports.SessionGateway{}
	if _, err := m.resolveAgentConfig(context.Background(), gateway, project); !errors.Is(err, ports.ErrModelCapabilitiesUnavailable) {
		t.Fatalf("non-gateway error = %v, want ErrModelCapabilitiesUnavailable", err)
	}
}
