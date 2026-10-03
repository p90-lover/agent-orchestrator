package sessionmanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Coding Tools can run any session's model through its local CPA gateway, so an
// agent such as Claude Code can use every model in the shared pool. The choice
// (provider and model) is kept per session in a small file beside the daemon's
// data, so it survives restores and interface switches without a schema change.
// The gateway key is read from the daemon's own environment at each launch and
// is never written to disk or inherited by agents under its own name.

// EnvCodingToolsCPAKey carries the CPA gateway key into the daemon.
const EnvCodingToolsCPAKey = "CODING_TOOLS_CPA_KEY"

// GatewayProviderCPA is the only gateway provider: CPA on its managed loopback port.
const GatewayProviderCPA = "cpa"

const (
	cpaGatewayBaseURL = "http://127.0.0.1:8317"
	gatewayFileName   = "coding-tools-gateways.json"
	maxGatewayEntries = 2000
)

// ValidateSessionGateway rejects any gateway other than CPA with a plain model id.
func ValidateSessionGateway(gateway ports.SessionGateway) error {
	if !gateway.Enabled() {
		return nil
	}
	if gateway.Provider != GatewayProviderCPA {
		return fmt.Errorf("unsupported model gateway %q", gateway.Provider)
	}
	model := strings.TrimSpace(gateway.Model)
	if model == "" || len(model) > 256 || model != gateway.Model ||
		strings.ContainsFunc(model, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("gateway model must be a plain model id")
	}
	return nil
}

// agyHelperModel is the internal model agy needs in its gateway list to build its default
// conversation config; agy sends it to the gateway as gemini-3.1-flash-lite, which CPA serves.
const agyHelperModel = "gemini-3.1-flash-lite-preview"

// codexGatewayConfig is the whole config.toml of the Codex home used by gateway sessions.
// Codex ignores OPENAI_BASE_URL when the user's own config picks a provider or a ChatGPT
// sign-in, so gateway sessions run from a home whose only setting is the CPA provider.
const codexGatewayConfig = `# Written by Agent Orchestrator for Coding Tools CPA gateway sessions.
model_provider = "coding_tools_cpa"

[model_providers.coding_tools_cpa]
name = "Coding Tools CPA"
base_url = "` + cpaGatewayBaseURL + `/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
`

// gatewayEnv points every supported agent at CPA with the chosen model. Each agent
// reads only its own family of variables and ignores the rest:
//   - Claude Code: ANTHROPIC_*
//   - Codex: CODEX_HOME (a home configured for CPA) with OPENAI_API_KEY
//   - opencode: OPENAI_*
//   - agy: AGY_LLM_GATEWAY_* (its gateway mode, which needs no Google sign-in; agy
//     sends its tools only for Gemini ids it knows, so other models can only chat)
//
// codexHome is empty when that home could not be prepared.
func gatewayEnv(gateway ports.SessionGateway, key, codexHome string) map[string]string {
	model := gateway.Model
	env := map[string]string{
		"AGY_LLM_GATEWAY_URL":            cpaGatewayBaseURL,
		"AGY_LLM_GATEWAY_API_KEY":        key,
		"AGY_LLM_GATEWAY_MODELS":         model + "," + agyHelperModel,
		"ANTHROPIC_BASE_URL":             cpaGatewayBaseURL,
		"ANTHROPIC_AUTH_TOKEN":           key,
		"ANTHROPIC_API_KEY":              key,
		"ANTHROPIC_MODEL":                model,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  model,
		"ANTHROPIC_SMALL_FAST_MODEL":     model,
		"OPENAI_BASE_URL":                cpaGatewayBaseURL + "/v1",
		"OPENAI_API_KEY":                 key,
	}
	if codexHome != "" {
		env["CODEX_HOME"] = codexHome
	}
	return env
}

// prepareCodexGatewayHome writes the CPA-only Codex home under the daemon's data dir and
// returns its path. It holds no secret: Codex reads the key from OPENAI_API_KEY.
func prepareCodexGatewayHome(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("daemon data dir is unset")
	}
	home := filepath.Join(dataDir, "gateway-homes", "codex")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	configPath := filepath.Join(home, "config.toml")
	if current, err := os.ReadFile(configPath); err == nil && string(current) == codexGatewayConfig {
		return home, nil
	}
	temp := configPath + ".tmp"
	if err := os.WriteFile(temp, []byte(codexGatewayConfig), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(temp, configPath); err != nil {
		return "", err
	}
	return home, nil
}

type gatewayStore struct {
	mu      sync.Mutex
	loaded  bool
	entries map[domain.SessionID]ports.SessionGateway
}

func (s *gatewayStore) path(dataDir string) string {
	return filepath.Join(dataDir, gatewayFileName)
}

func (s *gatewayStore) loadLocked(dataDir string) {
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[domain.SessionID]ports.SessionGateway{}
	if dataDir == "" {
		return
	}
	raw, err := os.ReadFile(s.path(dataDir))
	if err != nil {
		return
	}
	var saved map[domain.SessionID]ports.SessionGateway
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	for id, gateway := range saved {
		if gateway.Enabled() && ValidateSessionGateway(gateway) == nil {
			s.entries[id] = gateway
		}
	}
}

func (s *gatewayStore) get(dataDir string, id domain.SessionID) (ports.SessionGateway, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked(dataDir)
	gateway, ok := s.entries[id]
	return gateway, ok
}

func (s *gatewayStore) set(dataDir string, id domain.SessionID, gateway ports.SessionGateway) error {
	if err := ValidateSessionGateway(gateway); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked(dataDir)
	if gateway.Enabled() {
		s.entries[id] = gateway
	} else {
		delete(s.entries, id)
	}
	if len(s.entries) > maxGatewayEntries {
		return errors.New("too many gateway sessions recorded")
	}
	if dataDir == "" {
		return nil
	}
	raw, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	temp := s.path(dataDir) + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.path(dataDir))
}

// applySessionGateway adds the session's gateway variables to its launch env.
// The daemon's own copy of the key is always blanked so agents never inherit it.
func (m *Manager) applySessionGateway(env map[string]string, id domain.SessionID, caseInsensitive bool) {
	setProtectedEnv(env, EnvCodingToolsCPAKey, "", caseInsensitive)
	gateway, ok := m.gateways.get(m.dataDir, id)
	if !ok {
		return
	}
	key := strings.TrimSpace(os.Getenv(EnvCodingToolsCPAKey))
	if key == "" {
		m.logger.Warn("session uses the CPA gateway but the daemon has no gateway key; the agent will use its own sign-in",
			"session", id, "model", gateway.Model)
		return
	}
	codexHome, err := prepareCodexGatewayHome(m.dataDir)
	if err != nil {
		m.logger.Warn("Codex gateway home could not be prepared; Codex will use its own provider",
			"session", id, "error", err)
	}
	for name, value := range gatewayEnv(gateway, key, codexHome) {
		setProtectedEnv(env, name, value, caseInsensitive)
	}
}
