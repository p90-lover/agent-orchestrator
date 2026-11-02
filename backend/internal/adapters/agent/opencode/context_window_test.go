package opencode

import (
	"encoding/json"
	"testing"
)

func TestContextWindowConfigPreservesOverlaysAndBaseModel(t *testing.T) {
	existing := `{"permission":{"bash":"deny"},"agent":{"reviewer":{"permission":"deny"}},"provider":{"openai":{"options":{"baseURL":"http://127.0.0.1:8317/v1","apiKey":"env-key"},"models":{"gpt-5.5":{"limit":{"context":65536,"output":4096},"options":{"keep":true}}}}}}`
	content, err := PrepareContextWindowConfigContent(existing, "openai/gpt-5.5(high)", 32768)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(content), &config); err != nil {
		t.Fatal(err)
	}
	if config["permission"].(map[string]any)["bash"] != "deny" || config["agent"].(map[string]any)["reviewer"] == nil {
		t.Fatal("user/reviewer overlay was replaced")
	}
	provider := config["provider"].(map[string]any)["openai"].(map[string]any)
	if provider["options"].(map[string]any)["apiKey"] != "env-key" {
		t.Fatal("provider settings were replaced")
	}
	models := provider["models"].(map[string]any)
	base := models["gpt-5.5"].(map[string]any)["limit"].(map[string]any)
	selected := models["gpt-5.5(high)"].(map[string]any)["limit"].(map[string]any)
	if base["context"] != float64(65536) || selected["context"] != float64(32768) || selected["output"] != float64(4096) {
		t.Fatalf("selected/base limits=%+v/%+v", selected, base)
	}
}

func TestContextWindowConfigDoesNotInventUnknownLimits(t *testing.T) {
	content, err := PrepareContextWindowConfigContent("", "openai/gpt-5.5(high)", 0)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(content), &config); err != nil {
		t.Fatal(err)
	}
	entry := config["provider"].(map[string]any)["openai"].(map[string]any)["models"].(map[string]any)["gpt-5.5(high)"].(map[string]any)
	if entry["limit"] != nil {
		t.Fatalf("invented limits: %+v", entry)
	}
	for _, existing := range []string{"null", `{"provider":[]}`, `{"provider":{"openai":{"models":{"gpt-5.5":{"limit":[]}}}}}`} {
		if _, err := PrepareContextWindowConfigContent(existing, "openai/gpt-5.5", 32768); err == nil {
			t.Errorf("invalid overlay accepted: %s", existing)
		}
	}
}
