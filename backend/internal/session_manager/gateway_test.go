package sessionmanager

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("env[%s] = %q, want %q", key, env[key], value)
		}
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
