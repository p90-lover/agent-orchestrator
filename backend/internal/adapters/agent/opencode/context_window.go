package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// PrepareContextWindowConfigContent changes only the selected session model.
// A CPA suffix gets a private model entry so OpenCode can select its wire ID.
// Unknown numeric limits are never synthesized, and global files stay untouched.
func PrepareContextWindowConfigContent(existing, model string, tokens int64) (string, error) {
	if err := ports.ValidateContextWindow(domain.HarnessOpenCode, model, tokens); err != nil {
		return "", err
	}
	providerID, modelID, hasProvider := strings.Cut(model, "/")
	suffix := hasProvider && strings.HasSuffix(modelID, ")") && strings.Contains(modelID, "(")
	if tokens == 0 && !suffix {
		return existing, nil
	}
	if !hasProvider || providerID == "" || modelID == "" {
		return "", fmt.Errorf("OpenCode context tuning requires provider/model")
	}
	config := map[string]any{}
	if strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &config); err != nil || config == nil {
			return "", fmt.Errorf("opencode: invalid session OPENCODE_CONFIG_CONTENT")
		}
	}
	providers, err := contextConfigObject(config, "provider")
	if err != nil {
		return "", err
	}
	provider, err := contextConfigObject(providers, providerID)
	if err != nil {
		return "", err
	}
	models, err := contextConfigObject(provider, "models")
	if err != nil {
		return "", err
	}
	selected := map[string]any{}
	if value, exists := models[modelID]; exists {
		source, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("opencode: selected model configuration must be an object")
		}
		for key, value := range source {
			selected[key] = value
		}
	} else if suffix {
		baseID, _, _ := strings.Cut(modelID, "(")
		if source, ok := models[baseID].(map[string]any); ok {
			for key, value := range source {
				if key != "id" {
					selected[key] = value
				}
			}
		}
	}
	if id, ok := selected["id"].(string); ok && id != "" && id != modelID {
		return "", fmt.Errorf("opencode: selected model alias maps to a different wire ID")
	}
	if tokens > 0 {
		limit := map[string]any{}
		if value, exists := selected["limit"]; exists {
			source, ok := value.(map[string]any)
			if !ok {
				return "", fmt.Errorf("opencode: selected model limit must be an object")
			}
			for key, value := range source {
				limit[key] = value
			}
		}
		limit["context"] = tokens
		selected["limit"] = limit
	}
	models[modelID] = selected
	raw, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("opencode: encode session context configuration: %w", err)
	}
	return string(raw), nil
}

func contextConfigObject(parent map[string]any, key string) (map[string]any, error) {
	if value, exists := parent[key]; exists {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("opencode: context configuration %s must be an object", key)
		}
		return object, nil
	}
	object := map[string]any{}
	parent[key] = object
	return object, nil
}
