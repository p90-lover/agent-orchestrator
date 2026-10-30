package opencodeacp

import (
	"context"
	"encoding/json"
	"testing"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestConfigureCarriesSessionContextWindowAndKeepsUserProviderOverlay(t *testing.T) {
	_, env, err := configure(context.Background(), acpdriver.LaunchConfig{
		Model: "openai/gpt-5.5(high)", ContextWindow: 32768, Permissions: ports.PermissionModeDefault,
		Env: map[string]string{"OPENCODE_CONFIG_CONTENT": `{"permission":{"bash":"deny"},"provider":{"openai":{"options":{"baseURL":"http://127.0.0.1:8317/v1"}}}}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatal(err)
	}
	provider := config["provider"].(map[string]any)["openai"].(map[string]any)
	limit := provider["models"].(map[string]any)["gpt-5.5(high)"].(map[string]any)["limit"].(map[string]any)
	if limit["context"] != float64(32768) || provider["options"].(map[string]any)["baseURL"] != "http://127.0.0.1:8317/v1" ||
		config["permission"].(map[string]any)["bash"] != "deny" {
		t.Fatalf("context/provider/permission overlay=%+v", config)
	}
}
