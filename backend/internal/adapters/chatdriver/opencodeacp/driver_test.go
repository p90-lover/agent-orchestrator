package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSessionOptionsUseProviderAdvertisedModelOption(t *testing.T) {
	if got := sessionOptions(ports.ChatTurnSettings{}); got != nil {
		t.Fatalf("empty settings = %#v", got)
	}
	got := sessionOptions(ports.ChatTurnSettings{Model: "anthropic/claude-sonnet"})
	if len(got) != 1 || got[0].ID != "model" || got[0].Value != "anthropic/claude-sonnet" {
		t.Fatalf("model settings = %#v", got)
	}
}

func TestRejectsProviderNameBeforeLaunchingOpenCode(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "start"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			plugin := &unresolvedPlugin{}
			driver := New(plugin, nil)
			cfg := ports.ChatStartConfig{WorkspacePath: t.TempDir(), Model: "TensorMux"}
			var err error
			if resume {
				_, err = driver.Resume(context.Background(), ports.ChatResumeConfig{WorkspacePath: cfg.WorkspacePath, Model: cfg.Model, ProviderConversationID: "existing"})
			} else {
				_, err = driver.Start(context.Background(), cfg)
			}
			if !errors.Is(err, ports.ErrChatConfigOptionInvalid) || !strings.Contains(err.Error(), "provider/model") {
				t.Fatalf("error = %v, want model format validation with recovery guidance", err)
			}
			if plugin.resolved {
				t.Fatal("invalid model reached OpenCode binary resolution")
			}
		})
	}
}

type unresolvedPlugin struct{ resolved bool }

func (p *unresolvedPlugin) ResolveBinary(context.Context) (string, error) {
	p.resolved = true
	return "", errors.New("unexpected binary resolution")
}
func (*unresolvedPlugin) AuthStatus(context.Context) (ports.AgentAuthStatus, error) {
	return ports.AgentAuthStatusUnknown, nil
}

func TestValidateTurnSettingsModelFormat(t *testing.T) {
	for _, model := range []string{"", "tensormux/glm-4-7-flash", "openrouter/vendor/model", "custom-provider/private-model:latest"} {
		t.Run("valid/"+model, func(t *testing.T) {
			settings := ports.ChatTurnSettings{Model: model}
			if err := validateTurnSettings(ports.PermissionModeDefault, settings); err != nil {
				t.Fatal(err)
			}
			options := sessionOptions(settings)
			if model != "" && (len(options) != 1 || options[0].Value != model) {
				t.Fatalf("model ID changed: %#v", options)
			}
		})
	}
	for _, model := range []string{"TensorMux", "glm-4-7-flash", " ", "/model", "provider/", " /model", "provider/ "} {
		t.Run("invalid/"+model, func(t *testing.T) {
			err := validateTurnSettings(ports.PermissionModeDefault, ports.ChatTurnSettings{Model: model})
			if !errors.Is(err, ports.ErrChatConfigOptionInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSessionOptionsForwardsEffortAfterModel(t *testing.T) {
	got := sessionOptions(ports.ChatTurnSettings{Model: "opencode-go/deepseek-v4.1-flash", Effort: "max"})
	if len(got) != 2 || got[0].ID != "model" || got[1].ID != "effort" || got[1].Value != "max" {
		t.Fatalf("model+effort settings = %#v", got)
	}
	got = sessionOptions(ports.ChatTurnSettings{Effort: "xhigh"})
	if len(got) != 1 || got[0].ID != "effort" || got[0].Value != "xhigh" {
		t.Fatalf("effort-only settings = %#v", got)
	}
}

func TestConfigureInjectsThePermissionTiersOpenCodeEnforces(t *testing.T) {
	for _, test := range []struct {
		mode   ports.PermissionMode
		want   string
		prompt string
	}{
		{want: "ao-default"}, // No standing instructions still needs every tier.
		{mode: ports.PermissionModeDefault, want: "ao-default", prompt: "Follow AO worker rules."},
		{mode: ports.PermissionModeAcceptEdits, want: "ao-accept-edits", prompt: "Follow AO worker rules."},
		{mode: ports.PermissionModeAuto, want: "ao-auto", prompt: "Follow AO worker rules."},
		{mode: ports.PermissionModeBypassPermissions, want: "ao-bypass", prompt: "Follow AO worker rules."},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			args, env, err := configure(context.Background(), acpdriver.LaunchConfig{
				SessionID: "worker-1", SystemPrompt: test.prompt, Permissions: test.mode,
			}, nil, nil)
			if err != nil {
				t.Fatalf("configure: %v", err)
			}
			if len(args) != 1 || args[0] != "acp" {
				t.Fatalf("args = %#v", args)
			}
			var config struct {
				DefaultAgent string                    `json:"default_agent"`
				Agent        map[string]map[string]any `json:"agent"`
			}
			if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
				t.Fatalf("decode config: %v", err)
			}
			if config.DefaultAgent != test.want || len(config.Agent) != 4 {
				t.Fatalf("config = %#v, want default agent %q and four tiers", config, test.want)
			}
			// Every tier is advertised, so the picker can switch to any of them
			// mid-session, and each carries AO's standing instructions.
			for _, tier := range []string{"ao-default", "ao-accept-edits", "ao-auto", "ao-bypass"} {
				agent, ok := config.Agent[tier]
				if !ok {
					t.Fatalf("tier %q missing from %#v", tier, config.Agent)
				}
				if prompt, ok := agent["prompt"]; ok != (test.prompt != "") || (ok && prompt != test.prompt) {
					t.Fatalf("tier %q prompt = %#v, want %q (omitted when empty)", tier, prompt, test.prompt)
				}
			}
			// The asking tiers ask about the shell, the network and subagents,
			// which OpenCode's own default allows, so the chat sees them; default
			// asks about edits too. AO answers edits in accept edits itself.
			asked := map[string]any{"bash*": "ask", "webfetch*": "ask", "websearch*": "ask", "task*": "ask"}
			for _, tier := range []string{"ao-default", "ao-accept-edits", "ao-auto"} {
				want := map[string]any{}
				for key, value := range asked {
					want[key] = value
				}
				if tier == "ao-default" {
					want["edit*"] = "ask"
				}
				if got := config.Agent[tier]["permission"]; !reflect.DeepEqual(got, want) {
					t.Fatalf("tier %q permission = %#v, want %#v", tier, got, want)
				}
			}
			// OpenCode's scalar full-access form: a wildcard rule can still lose
			// to a more specific deny contributed by another config layer.
			if got := config.Agent["ao-bypass"]["permission"]; got != "allow" {
				t.Fatalf("bypass tier = %#v", got)
			}
		})
	}
}

func TestConfigureKeepsTheUsersOwnInlineConfig(t *testing.T) {
	_, env, err := configure(context.Background(), acpdriver.LaunchConfig{
		SessionID: "worker-2", Permissions: ports.PermissionModeDefault,
		Env: map[string]string{
			"OPENCODE_CONFIG_CONTENT": `{"provider":{"local":{"name":"Local"}},"agent":{"mine":{"mode":"primary"}},"permission":{"bash":"deny"}}`,
		},
	}, nil, nil)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	var config struct {
		Provider   map[string]any            `json:"provider"`
		Permission map[string]any            `json:"permission"`
		Agent      map[string]map[string]any `json:"agent"`
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config.Provider["local"] == nil || config.Agent["mine"] == nil || config.Permission["bash"] != "deny" {
		t.Fatalf("user config was not preserved: %#v", config)
	}
}

func TestConfigureRestatesTheUsersOwnRulesAfterAOsAsks(t *testing.T) {
	var asked acpdriver.LaunchConfig
	read := func(_ context.Context, cfg acpdriver.LaunchConfig) (json.RawMessage, error) {
		asked = cfg
		return json.RawMessage(`{"bash":{"rm *":"deny"}}`), nil
	}
	_, env, err := configure(context.Background(), acpdriver.LaunchConfig{
		SessionID: "worker-3", WorkspacePath: "G:/repo", Permissions: ports.PermissionModeBypassPermissions,
	}, read, nil)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	// Read whatever mode the session starts in: the picker can move a bypass
	// session to an asking tier later.
	if asked.WorkspacePath != "G:/repo" {
		t.Fatalf("the user's rules were not read for this workspace: %#v", asked)
	}
	var config struct {
		Agent map[string]struct {
			Permission json.RawMessage `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatal(err)
	}
	want := `{"bash*":"ask","webfetch*":"ask","websearch*":"ask","task*":"ask","bash":{"rm *":"deny"}}`
	if got := string(config.Agent["ao-accept-edits"].Permission); got != want {
		t.Fatalf("accept edits = %s, want %s", got, want)
	}
}

func TestConfigureAsksWithoutTheUsersRulesWhenOpenCodeCannotGiveThem(t *testing.T) {
	for name, read := range map[string]permissionReader{
		"failed": func(context.Context, acpdriver.LaunchConfig) (json.RawMessage, error) {
			return nil, errors.New("opencode debug config: exit status 1")
		},
		"unreadable": func(context.Context, acpdriver.LaunchConfig) (json.RawMessage, error) {
			return json.RawMessage(`[1]`), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logged bytes.Buffer
			_, env, err := configure(context.Background(), acpdriver.LaunchConfig{
				SessionID: "worker-4", Permissions: ports.PermissionModeAcceptEdits,
			}, read, slog.New(slog.NewTextHandler(&logged, nil)))
			if err != nil {
				t.Fatalf("configure: %v", err)
			}
			if !strings.Contains(env["OPENCODE_CONFIG_CONTENT"], `"permission":{"bash*":"ask","webfetch*":"ask","websearch*":"ask","task*":"ask"}`) {
				t.Fatalf("accept edits does not ask: %s", env["OPENCODE_CONFIG_CONTENT"])
			}
			if !strings.Contains(logged.String(), "could not read the user's permission rules") {
				t.Fatalf("no warning logged: %q", logged.String())
			}
		})
	}
}

func TestPermissionFromDebugConfig(t *testing.T) {
	for _, test := range []struct {
		name, output, want string
		fails              bool
	}{
		{name: "after a notice", output: "Installing plugins\n{\n  \"$schema\": \"x\",\n  \"permission\": {\n    \"*\": \"deny\",\n    \"bash\": \"allow\"\n  },\n  \"provider\": {}\n}\n", want: `{
    "*": "deny",
    "bash": "allow"
  }`},
		{name: "none set", output: `{"provider":{}}`},
		{name: "no config", output: "error: something", fails: true},
		{name: "cut short", output: `{"permission":{"bash":`, fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := permissionFromDebugConfig([]byte(test.output))
			if (err != nil) != test.fails || string(got) != test.want {
				t.Fatalf("permissionFromDebugConfig = (%s, %v), want (%s, fails %v)", got, err, test.want, test.fails)
			}
		})
	}
}

func TestPermissionPolicyAnswersRequestsTheWayAutoDoes(t *testing.T) {
	edit, execute, fetch := acpsdk.ToolKindEdit, acpsdk.ToolKindExecute, acpsdk.ToolKindFetch
	options := []acpsdk.PermissionOption{
		{OptionId: "reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
		{OptionId: "once", Kind: acpsdk.PermissionOptionKindAllowOnce},
		{OptionId: "always", Kind: acpsdk.PermissionOptionKindAllowAlways},
	}
	for _, test := range []struct {
		name   string
		mode   ports.PermissionMode
		kind   *acpsdk.ToolKind
		wantID acpsdk.PermissionOptionId
	}{
		{name: "default parks an edit", mode: ports.PermissionModeDefault, kind: &edit},
		{name: "accept edits answers an edit", mode: ports.PermissionModeAcceptEdits, kind: &edit, wantID: "once"},
		{name: "accept edits parks a command", mode: ports.PermissionModeAcceptEdits, kind: &execute},
		{name: "auto answers a command", mode: ports.PermissionModeAuto, kind: &execute, wantID: "once"},
		{name: "auto answers a fetch", mode: ports.PermissionModeAuto, kind: &fetch, wantID: "once"},
		{name: "auto answers an unclassified request", mode: ports.PermissionModeAuto, wantID: "once"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, handled := permissionPolicy(test.mode, acpsdk.RequestPermissionRequest{
				ToolCall: acpsdk.ToolCallUpdate{Kind: test.kind}, Options: options,
			})
			if id != test.wantID || handled != (test.wantID != "") {
				t.Fatalf("selection = (%q, %v), want (%q, %v)", id, handled, test.wantID, test.wantID != "")
			}
		})
	}

	// A denied tool never raises a request, so nothing here can override it —
	// which is the whole reason AO stopped writing rules for these modes.
	if _, handled := permissionPolicy(ports.PermissionModeAuto,
		acpsdk.RequestPermissionRequest{Options: options[:1]}); handled {
		t.Fatal("auto answered a request offering no allow-once option")
	}
}
