package piacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const gatewayKeyEnv = "AO_PI_GATEWAY_API_KEY"

func prepareGatewayCatalog(ctx context.Context, cfg acpdriver.LaunchConfig, env map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	model := strings.TrimSpace(env["AO_PI_GATEWAY_MODEL"])
	url := strings.TrimSpace(env["AO_PI_GATEWAY_URL"])
	if model == "" || url == "" || cfg.Model != ports.PiGatewayProvider+"/"+model {
		return nil
	}
	if url != ports.CPAGatewayBaseURL+"/v1" || strings.TrimSpace(env[gatewayKeyEnv]) == "" {
		return errors.New("Pi's Coding Tools model needs the local CPA gateway and its current credential")
	}
	window, err := strconv.Atoi(env["AO_PI_GATEWAY_CONTEXT_WINDOW"])
	if err != nil || window <= 0 {
		window = 200000
	}
	// Pi ACP checks for a configured model in session/new, before AO can select one.
	// A gateway-only agent directory keeps CPA credentials and provider choices out of Pi's normal profile.
	dir := filepath.Join(cfg.DataDir, "pi-acp", string(cfg.SessionID), "gateway-agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare Pi's gateway-only agent directory: %w", err)
	}
	catalog := map[string]any{"providers": map[string]any{ports.PiGatewayProvider: map[string]any{
		"baseUrl": url, "api": "openai-completions", "apiKey": gatewayKeyEnv,
		"models": []map[string]any{{
			"id": model, "name": model, "reasoning": false, "input": []string{"text", "image"},
			"contextWindow": window, "maxTokens": min(window, 32000),
		}},
	}}}
	data, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("encode Pi's gateway model catalog: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "models.json"), data, 0o600); err != nil {
		return fmt.Errorf("write Pi's gateway model catalog: %w", err)
	}
	env["PI_CODING_AGENT_DIR"] = dir
	return nil
}
