// Package opencodeacp binds the user's own OpenCode installation to AO's
// reusable ACP Chat transport.
package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/opencode"
	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/nativeacp"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/opencodeidentity"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// New launches `opencode acp` from the exact binary resolved by the existing
// OpenCode agent plugin. AO adds only a per-session inline overlay for its
// standing instructions and an explicit bypass-permissions choice.
func New(plugin nativeacp.Plugin, log *slog.Logger) ports.ChatDriver {
	readPermission := func(ctx context.Context, cfg acpdriver.LaunchConfig) (json.RawMessage, error) {
		binary, err := plugin.ResolveBinary(ctx)
		if err != nil {
			return nil, err
		}
		return readUserPermission(ctx, binary, cfg)
	}
	return nativeacp.New(plugin, nativeacp.Config{
		Harness: domain.HarnessOpenCode,
		Configure: func(ctx context.Context, cfg acpdriver.LaunchConfig) ([]string, map[string]string, error) {
			return configure(ctx, cfg, readPermission, log)
		},
		SessionOptions:       sessionOptions,
		PermissionPolicy:     permissionPolicy,
		ValidateTurnSettings: validateTurnSettings,
		DecodeProviderConversationID: func(providerID string) (string, error) {
			return opencodeidentity.Decode(domain.HarnessOpenCode, providerID)
		},
	}, log)
}

// permissionReader returns the user's own resolved OpenCode `permission`
// config for a launch (readUserPermission).
type permissionReader func(context.Context, acpdriver.LaunchConfig) (json.RawMessage, error)

func configure(ctx context.Context, cfg acpdriver.LaunchConfig, readPermission permissionReader, log *slog.Logger) ([]string, map[string]string, error) {
	contextContent, err := opencode.PrepareContextWindowConfigContent(cfg.Env["OPENCODE_CONFIG_CONTENT"], cfg.Model, cfg.ContextWindow)
	if err != nil {
		return nil, nil, err
	}
	// Every tier is written at launch (the mode picker switches between them
	// mid-session), so the user's rules are read whatever mode this starts in.
	var userPermission json.RawMessage
	if readPermission != nil {
		userPermission, err = readPermission(ctx, cfg)
		if err == nil {
			err = opencode.CheckPermissionConfig(userPermission)
		}
		if err != nil {
			// The asking tiers then ask without the user's rules after them, so
			// a tool the user denied is asked about rather than refused: still
			// never run unseen, and the chat shows the request.
			userPermission = nil
			if log != nil {
				log.Warn("opencode: could not read the user's permission rules; the asking modes ask without them",
					"session_id", cfg.SessionID, "error", err)
			}
		}
	}
	content, err := opencode.PrepareACPConfigContent(
		contextContent, cfg.SystemPrompt, cfg.Permissions, userPermission)
	if err != nil {
		return nil, nil, err
	}
	// RTK's plugin for OpenCode's own bash tool, when Coding Tools passed one.
	if content, err = withRTKPlugin(content, cfg.Env); err != nil {
		return nil, nil, err
	}
	return []string{"acp"}, map[string]string{"OPENCODE_CONFIG_CONTENT": content}, nil
}

func sessionOptions(settings ports.ChatTurnSettings) []acpdriver.SessionOption {
	options := make([]acpdriver.SessionOption, 0, 2)
	if settings.Model != "" {
		options = append(options, acpdriver.SessionOption{ID: "model", Value: settings.Model})
	}
	// OpenCode advertises effort as id "effort" (category "thought_level") with
	// the model's variant names as values. The model setter resets the variant
	// to "default" else the first variant when no explicit variant is given,
	// so the model must be applied first and the effort second.
	if settings.Effort != "" {
		options = append(options, acpdriver.SessionOption{ID: "effort", Value: settings.Effort})
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

// OpenCode parses model overrides as provider/model. A provider display name is
// not an alias for its configured default model; keep that default only when
// the override is empty. Leave model availability to the user's OpenCode.
func validateTurnSettings(_ ports.PermissionMode, settings ports.ChatTurnSettings) error {
	if settings.Model == "" {
		return nil
	}
	provider, model, found := strings.Cut(settings.Model, "/")
	if !found || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
		return fmt.Errorf("%w: OpenCode model %q must use provider/model format (for example, anthropic/claude-sonnet); select a full model ID from `opencode models`, or clear the model override to use agent settings", ports.ErrChatConfigOptionInvalid, settings.Model)
	}
	return nil
}

// userPermissionTimeout bounds `opencode debug config` at launch. In a fresh
// config folder OpenCode first installs its plugin package, which can take
// minutes; the launch goes on without the user's rules rather than wait.
const userPermissionTimeout = 20 * time.Second

// readUserPermission asks OpenCode itself for the user's resolved `permission`
// config (`opencode debug config`, run where and with the env the session
// gets), so every layer counts as OpenCode merges them: global, custom,
// project and .opencode files, OPENCODE_CONFIG_CONTENT, OPENCODE_PERMISSION,
// managed and remote config. Reading the files in AO could not reproduce that
// (AO tried and dropped it upstream). Only `permission` is kept; the rest of the
// output, which can hold provider credentials, is never logged.
func readUserPermission(ctx context.Context, binary string, cfg acpdriver.LaunchConfig) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, userPermissionTimeout)
	defer cancel()
	cmd := aoprocess.CommandContext(ctx, binary, "debug", "config")
	cmd.Dir = cfg.WorkspacePath
	cmd.Env = processenv.Merge(cfg.Env)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("opencode debug config: %w", ctx.Err())
		}
		return nil, fmt.Errorf("opencode debug config: %w", err)
	}
	return permissionFromDebugConfig(stdout.Bytes())
}

// permissionFromDebugConfig picks `permission` out of `opencode debug config`
// output: the resolved config as one JSON object, after anything OpenCode
// printed first. Empty when the config sets none.
func permissionFromDebugConfig(output []byte) (json.RawMessage, error) {
	start := bytes.IndexByte(output, '{')
	if start < 0 {
		return nil, fmt.Errorf("opencode debug config printed no config")
	}
	var config struct {
		Permission json.RawMessage `json:"permission"`
	}
	if err := json.NewDecoder(bytes.NewReader(output[start:])).Decode(&config); err != nil {
		return nil, fmt.Errorf("opencode debug config: decode: %w", err)
	}
	return config.Permission, nil
}

// permissionPolicy is AO's side of accept-edits and auto, and mirrors how
// OpenCode implements --auto: reply to each permission request rather than
// granting anything up front. A tool the user or repository denied never
// raises a request (their rules are restated after AO's asks), so provider
// policy stays authoritative. Every other request in default, and all but
// edits in accept edits, wait for the chat: the person on Ask for approval,
// the app's reviewer on Approve for me. Bypass needs no entry — its agent
// allows everything, so nothing is asked.
func permissionPolicy(
	mode ports.PermissionMode,
	params acpsdk.RequestPermissionRequest,
) (acpsdk.PermissionOptionId, bool) {
	mode = ports.NormalizePermissionMode(mode)
	kind := params.ToolCall.Kind
	if mode != ports.PermissionModeAuto && (mode != ports.PermissionModeAcceptEdits || kind == nil ||
		(*kind != acpsdk.ToolKindEdit && *kind != acpsdk.ToolKindDelete && *kind != acpsdk.ToolKindMove)) {
		return "", false
	}
	// Once, not always: --auto answers each request and persists nothing.
	for _, option := range params.Options {
		if option.Kind == acpsdk.PermissionOptionKindAllowOnce {
			return option.OptionId, true
		}
	}
	return "", false
}
