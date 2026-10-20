// Package claudeacp binds Claude Code to AO's generic ACP Chat driver.
//
// AO ships the protocol adapter and its Node runtime, not Claude Code. The
// adapter receives CLAUDE_CODE_EXECUTABLE pointing at the same user-installed
// binary used by AO's existing TUI adapter, so login, subscription, settings,
// MCP configuration, hooks, and project instructions remain the user's own.
package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const minimumNodeMajor = 22

type claudePlugin interface {
	ResolveBinary(context.Context) (string, error)
}

type claudeLaunchAuthenticator interface {
	ValidateLaunchAuth(context.Context, string, map[string]string) (ports.AgentAuthStatus, error)
}

// New constructs the Claude Code ACP driver over the existing Claude agent
// plugin. The plugin remains the canonical discovery/auth implementation for
// both Chat and TUI modes.
func New(plugin claudePlugin, log *slog.Logger, onAuthRejected func()) ports.ChatDriver {
	return &checkpointDriver{plugin: plugin, ChatDriver: acpdriver.New(acpdriver.Config{
		Harness: domain.HarnessClaudeCode,
		// A live rejection is the ground truth that outranks any cached
		// verdict, so drop the cache the moment one arrives. This is also the
		// only auth correction that works for credential sources AO cannot
		// read at all — the Bedrock and Vertex chains — because it needs no
		// credential, no network call, and no provider knowledge.
		OnAuthRejected:        claudeAuthRejected(onAuthRejected),
		PromptResponseFailure: claudePromptResponseFailure,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming:    true,
			ports.ChatCapabilityTools:        true,
			ports.ChatCapabilityApprovals:    true,
			ports.ChatCapabilityInterrupt:    true,
			ports.ChatCapabilityResume:       true,
			ports.ChatCapabilityPromptReplay: true,
			ports.ChatCapabilityUsage:        true,
			ports.ChatCapabilityDiffs:        true,
			ports.ChatCapabilityPlans:        true,
			ports.ChatCapabilityCompaction:   true,
		},
		Probe: func(ctx context.Context) error {
			if _, err := resolveRuntime(ctx); err != nil {
				return fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			claudeBinary, err := plugin.ResolveBinary(ctx)
			if err != nil {
				return fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			if err := validateClaudeACPExecutable(claudeBinary, runtime.GOOS); err != nil {
				return fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			return nil
		},
		Launch: func(ctx context.Context, cfg acpdriver.LaunchConfig) (acpdriver.Launch, error) {
			runtimeLaunch, err := resolveRuntime(ctx)
			if err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			claudeBinary, err := plugin.ResolveBinary(ctx)
			if err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			if err := validateClaudeACPExecutable(claudeBinary, runtime.GOOS); err != nil {
				return acpdriver.Launch{}, fmt.Errorf("%w: %w", ports.ErrChatDriverUnavailable, err)
			}
			// A CPA gateway session talks to AO's own local gateway with AO's key and a
			// model fixed in its environment: `claude auth status`, the credential probe
			// and provider model discovery would only describe that gateway, at the cost
			// of cold Claude CLI starts before every launch. Its first request reports a
			// gateway failure anyway.
			gatewaySession := strings.TrimRight(cfg.Env["ANTHROPIC_BASE_URL"], "/") == ports.CPAGatewayBaseURL
			// The auth check and model discovery each start the Claude CLI
			// (`claude auth status`) cold; run side by side, a launch waits for
			// one of them instead of both in a row.
			var models []ports.AgentModelInfo
			modelsDone := make(chan struct{})
			modelCtx, cancelModels := context.WithCancel(ctx)
			defer cancelModels()
			if _, preserve := claudeACPModelConfig(cfg.Env); !preserve && !gatewaySession {
				go func() {
					defer close(modelsDone)
					var modelErr error
					models, modelErr = claudecode.ProviderModels(modelCtx, claudeBinary, cfg.WorkspacePath, cfg.Env)
					if modelErr != nil && log != nil {
						log.Debug("Claude provider model discovery unavailable; using ACP defaults", "error", modelErr)
					}
				}()
			} else {
				close(modelsDone)
			}
			if !gatewaySession {
				if err := validateClaudeLaunchAuth(ctx, plugin, cfg.WorkspacePath, cfg.Env, log); err != nil {
					cancelModels()
					<-modelsDone
					return acpdriver.Launch{}, err
				}
			}
			<-modelsDone
			env := claudeACPLaunchEnv(cfg.Env, claudeBinary, cfg.Model, models)
			if err := claudecode.ApplyContextWindowEnv(ctx, claudeBinary, cfg.Model, cfg.ContextWindow, env); err != nil {
				return acpdriver.Launch{}, err
			}
			return acpdriver.Launch{
				Command: runtimeLaunch.command,
				Args:    runtimeLaunch.args,
				Env:     env,
			}, nil
		},
		SessionMeta:    claudeSessionMeta,
		SessionMode:    claudeSessionMode,
		SessionOptions: claudeSessionOptions,
	}, log)}
}

func validateClaudeLaunchAuth(ctx context.Context, plugin claudePlugin, workingDir string, env map[string]string, log *slog.Logger) error {
	validator, ok := plugin.(claudeLaunchAuthenticator)
	if !ok {
		return nil
	}
	status, err := validator.ValidateLaunchAuth(ctx, workingDir, env)
	if err != nil {
		if log != nil {
			log.Debug("Claude launch auth probe inconclusive; continuing", "error", err)
		}
		return nil
	}
	if status == ports.AgentAuthStatusUnauthorized {
		return ports.ErrAgentAuthRequired
	}
	return nil
}

func claudeAuthRejected(notifyDaemon func()) func() {
	return func() {
		claudecode.InvalidateAuthCache()
		if notifyDaemon != nil {
			notifyDaemon()
		}
	}
}

func claudePromptResponseFailure(response acpsdk.PromptResponse) error {
	if response.StopReason != acpsdk.StopReasonEndTurn {
		return nil
	}
	air := claudeNestedMap(claudeNestedMap(response.Meta, "jetbrains"), "air")
	version, versionOK := air["version"].(float64)
	failure := claudeNestedMap(air, "sessionFailure")
	id, _ := failure["id"].(string)
	title, _ := failure["title"].(string)
	if !versionOK || version < 1 || strings.TrimSpace(id) == "" || strings.TrimSpace(title) == "" || failure["severity"] != "error" {
		return nil
	}
	details, _ := failure["details"].(string)
	var cause error
	if actions, ok := failure["actions"].([]any); ok {
		for _, action := range actions {
			if action == "login" {
				cause = ports.ErrChatAuthRequired
				break
			}
		}
	}
	return ports.NewChatProviderFailure(strings.TrimSpace(title), strings.TrimSpace(details), cause)
}

func claudeNestedMap(meta map[string]any, key string) map[string]any {
	if meta == nil {
		return nil
	}
	value, _ := meta[key].(map[string]any)
	return value
}

// claudeACPLaunchEnv gives claude-agent-acp the same provider model IDs AO
// exposes in its pre-launch picker. The adapter turns availableModels into its
// authoritative ACP model choices, so a raw first-party ID selected in AO is
// accepted by session/set_config_option instead of being rejected because the
// adapter started with aliases only.
func claudeACPLaunchEnv(
	input map[string]string,
	binary string,
	selectedModel string,
	models []ports.AgentModelInfo,
) map[string]string {
	env := make(map[string]string, len(input)+2)
	for key, value := range input {
		env[key] = value
	}
	// This prevents the adapter's optional native Claude package from becoming
	// a second installation managed by AO.
	env["CLAUDE_CODE_EXECUTABLE"] = binary
	selected := strings.TrimSpace(selectedModel)
	if _, configured := input["ANTHROPIC_CUSTOM_MODEL_OPTION"]; selected != "" &&
		!isClaudeNativeModelAlias(selected) && !configured &&
		strings.TrimSpace(os.Getenv("ANTHROPIC_CUSTOM_MODEL_OPTION")) == "" {
		// availableModels restricts Claude Code's built-in picker but does not
		// make every provider-discovered API ID a selectable SDK model. The
		// custom option is the supported bridge for the one API model AO is
		// actually starting this session with.
		env["ANTHROPIC_CUSTOM_MODEL_OPTION"] = selected
	}
	config, preserve := claudeACPModelConfig(input)
	if preserve {
		return env
	}

	ids := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	// With no provider catalog, leave native aliases to claude-agent-acp. An
	// availableModels list containing only the selected alias would replace its
	// full built-in picker with that single model.
	if len(ids) == 0 && isClaudeNativeModelAlias(selected) {
		return env
	}
	if selected != "" {
		if _, exists := seen[selected]; !exists {
			ids = append(ids, selected)
		}
	}
	if len(ids) == 0 {
		return env
	}

	encodedIDs, err := json.Marshal(ids)
	if err != nil {
		return env
	}
	config["availableModels"] = encodedIDs
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return env
	}
	env["CLAUDE_MODEL_CONFIG"] = string(encodedConfig)
	return env
}

func isClaudeNativeModelAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "default", "sonnet", "opus", "haiku", "fable", "opus[1m]":
		return true
	default:
		return false
	}
}

// claudeACPModelConfig returns a mergeable user configuration. preserve is
// true when AO must pass the value through untouched, either because the user
// supplied an authoritative availableModels list or because ACP should report
// malformed configuration itself. Callers can also use preserve to avoid a
// provider lookup whose result would be discarded.
func claudeACPModelConfig(input map[string]string) (map[string]json.RawMessage, bool) {
	raw, configured := input["CLAUDE_MODEL_CONFIG"]
	if !configured {
		raw = os.Getenv("CLAUDE_MODEL_CONFIG")
	}
	config := make(map[string]json.RawMessage)
	if strings.TrimSpace(raw) == "" {
		return config, false
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, true
	}
	if config == nil {
		return nil, true
	}
	_, userRestricted := config["availableModels"]
	return config, userRestricted
}

func validateClaudeACPExecutable(binary, goos string) error {
	if goos != "windows" {
		return nil
	}
	switch strings.ToLower(filepath.Ext(binary)) {
	case ".cmd", ".bat":
		return fmt.Errorf("resolved Claude Code command shim %q, but chat requires the native claude.exe; reinstall or update Claude Code", binary)
	default:
		return nil
	}
}

func claudeSessionMeta(cfg acpdriver.LaunchConfig) map[string]any {
	meta := map[string]any{}
	if standing := strings.TrimSpace(cfg.SystemPrompt); standing != "" {
		// Append AO's standing instructions to Claude Code's own prompt. Replacing
		// the preset would discard Claude's native coding/tool instructions.
		meta["systemPrompt"] = map[string]any{
			"type": "preset", "preset": "claude_code", "append": standing,
		}
	}
	// An AO session runs unattended, so it reads only the project's and the
	// worktree's settings, never the person's own ~/.claude settings: their output
	// style ("Learning" made a reviewer ask the person what to do instead of
	// reviewing) and their hooks are for their interactive Claude Code. Routing,
	// models, permissions and tools come from AO itself.
	options := map[string]any{"settingSources": []string{"project", "local"}}
	// Newer Claude models return thinking blocks with no text unless the request
	// asks for summarized display, so a chat could only say "Thought". Ask for
	// summaries on the native API, where adaptive thinking is already the
	// default. Through a gateway (CPA) the thinking setting would also change
	// the effort; the gateway requests summaries itself instead.
	if strings.TrimSpace(cfg.Env["ANTHROPIC_BASE_URL"]) == "" {
		options["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
	}
	// The chosen effort, from the session's start. The ACP adapter offers an
	// effort option only for models Claude Code recognises, so through CPA a
	// model such as gemini-3.8-flash-high got none, the per-turn option was
	// never applied, and every request went out at Claude Code's default
	// ("medium") whatever the chat picked. Supported models still take later
	// changes through that option.
	if effort := claudeSDKEffort(cfg.Effort); effort != "" {
		options["effort"] = effort
	}
	// RTK (github.com/rtk-ai/rtk) rewrites the agent's Bash commands to compact
	// equivalents, so the model reads a fraction of their output. Coding Tools
	// passes RTK's hook command while RTK is installed and turned on; it goes into
	// this session's own flag settings, never the person's Claude settings.
	if hook := claudeRTKHook(cfg.Env); hook != "" {
		options["settings"] = map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"type": "command", "command": hook}},
		}}}}
	}
	meta["claudeCode"] = map[string]any{"options": options}
	return meta
}

// EnvRTKClaudeHook carries RTK's Claude Code hook command ("<rtk> hook claude")
// from Coding Tools. It is read from the session's environment, then the
// daemon's.
const EnvRTKClaudeHook = "CODING_TOOLS_RTK_CLAUDE_HOOK"

// claudeRTKHook is the RTK hook command to register, or "" for none or one
// that is not a single plain command line.
func claudeRTKHook(env map[string]string) string {
	hook, ok := env[EnvRTKClaudeHook]
	if !ok {
		hook = os.Getenv(EnvRTKClaudeHook)
	}
	hook = strings.TrimSpace(hook)
	if hook == "" || len(hook) > 1024 || strings.ContainsFunc(hook, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	return hook
}

// claudeSDKEffort is the Agent SDK effort level for AO's value, or "" for none
// or one the SDK does not take.
func claudeSDKEffort(effort string) string {
	switch value := strings.ToLower(strings.TrimSpace(effort)); value {
	case "low", "medium", "high", "xhigh", "max":
		return value
	default:
		return ""
	}
}

func claudeSessionMode(permission ports.PermissionMode) string {
	switch ports.NormalizePermissionMode(permission) {
	case ports.PermissionModeAcceptEdits:
		return "acceptEdits"
	case ports.PermissionModeAuto:
		return "auto"
	case ports.PermissionModeBypassPermissions:
		return "bypassPermissions"
	default:
		return ""
	}
}

func claudeSessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	options := make([]acpdriver.SessionOption, 0, 2)
	if settings.Model != "" {
		options = append(options, acpdriver.SessionOption{ID: "model", Value: settings.Model})
	}
	if settings.Effort != "" {
		options = append(options, acpdriver.SessionOption{ID: "effort", Value: settings.Effort})
	}
	return options
}

type runtimeLaunch struct {
	command string
	args    []string
}

// resolveRuntime finds AO's packaged ACP runtime. Explicit command and path
// overrides keep headless development/test installs usable without coupling the
// backend to Electron's directory layout.
func resolveRuntime(ctx context.Context) (runtimeLaunch, error) {
	if err := ctx.Err(); err != nil {
		return runtimeLaunch{}, err
	}
	if command := strings.TrimSpace(os.Getenv("AO_CLAUDE_ACP_COMMAND")); command != "" {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return runtimeLaunch{}, fmt.Errorf("resolve AO_CLAUDE_ACP_COMMAND %q: %w", command, err)
		}
		return runtimeLaunch{command: resolved}, nil
	}

	runtimeDir := strings.TrimSpace(os.Getenv("AO_ACP_RUNTIME_DIR"))
	if runtimeDir == "" {
		runtimeDir = runtimeDirectoryBesideExecutable()
	}
	if runtimeDir == "" {
		return runtimeLaunch{}, errors.New("AO ACP runtime is not installed")
	}
	node := filepath.Join(runtimeDir, "node", "bin", "node")
	if runtime.GOOS == "windows" {
		node = filepath.Join(runtimeDir, "node", "node.exe")
	}
	entry := filepath.Join(runtimeDir, "node_modules", "@agentclientprotocol", "claude-agent-acp", "dist", "index.js")
	if err := requireFile(node, "packaged Node runtime"); err != nil {
		return runtimeLaunch{}, err
	}
	if err := requireFile(entry, "claude-agent-acp entrypoint"); err != nil {
		return runtimeLaunch{}, err
	}
	if err := requireNodeVersion(ctx, node); err != nil {
		return runtimeLaunch{}, err
	}
	return runtimeLaunch{command: node, args: []string{entry}}, nil
}

func runtimeDirectoryBesideExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	resources := filepath.Dir(filepath.Dir(executable))
	for _, candidate := range []string{
		filepath.Join(resources, "acp-runtime"),
		filepath.Join(resources, "resources", "acp-runtime"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func requireFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s at %s: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s at %s is a directory", label, path)
	}
	return nil
}

// verifiedNodes remembers each packaged Node executable (path, size, mtime)
// that passed requireNodeVersion. Running `node --version` cost every chat
// launch a cold process start, seconds on a busy machine, for an answer that
// changes only when the runtime is replaced, which changes the key.
var verifiedNodes sync.Map // nodeKey -> struct{}

type nodeKey struct {
	path    string
	size    int64
	modTime int64
}

func requireNodeVersion(ctx context.Context, node string) error {
	var key nodeKey
	if info, err := os.Stat(node); err == nil {
		key = nodeKey{path: node, size: info.Size(), modTime: info.ModTime().UnixNano()}
		if _, ok := verifiedNodes.Load(key); ok {
			return nil
		}
	}
	if err := checkNodeVersion(ctx, node); err != nil {
		return err
	}
	if key.path != "" {
		verifiedNodes.Store(key, struct{}{})
	}
	return nil
}

func checkNodeVersion(ctx context.Context, node string) error {
	// node is the explicit AO override or the validated executable inside AO's
	// packaged resources, never prompt/provider input.
	out, err := aoprocess.CommandContext(ctx, node, "--version").Output() //nolint:gosec // Resolved local executable, not provider input.
	if err != nil {
		return fmt.Errorf("run packaged Node: %w", err)
	}
	version := strings.TrimSpace(strings.TrimPrefix(string(out), "v"))
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < minimumNodeMajor {
		return fmt.Errorf("claude-agent-acp requires Node %d+; found %q", minimumNodeMajor, strings.TrimSpace(string(out)))
	}
	return nil
}
