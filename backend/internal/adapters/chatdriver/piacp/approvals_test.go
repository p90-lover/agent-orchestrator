package piacp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestLaunchGivesTheDaemonTheSessionsApprovalSettings(t *testing.T) {
	dataDir := t.TempDir()
	executable = func() (string, error) { return filepath.Join(dataDir, "bin", "ao.exe"), nil }
	t.Cleanup(func() { executable = os.Executable })
	cfg := buildConfig(fakePiPlugin{}, func(context.Context) (string, error) { return "pi-acp", nil }, skipDaemon, nil)
	launchCfg := acpdriver.LaunchConfig{SessionID: "session-7", DataDir: dataDir, WorkspacePath: dataDir}

	launch, err := cfg.Launch(context.Background(), launchCfg)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	token := launch.Env[envApprovalToken]
	if len(token) != 64 || launch.Env[envApprovalSessionID] != "session-7" ||
		launch.Env[envApprovalAOBinary] != filepath.Join(dataDir, "bin", "ao.exe") {
		t.Fatalf("approval env = %q %q %q", launch.Env[envApprovalSessionID], token, launch.Env[envApprovalAOBinary])
	}
	// The conversation gets the same token, and a relaunch (a daemon that
	// outlived AO keeps its env) gets it again.
	if got, err := cfg.ToolApprovalToken(launchCfg); err != nil || got != token {
		t.Fatalf("conversation token = %q, %v; want the daemon's", got, err)
	}
	again, err := cfg.Launch(context.Background(), launchCfg)
	if err != nil || again.Env[envApprovalToken] != token {
		t.Fatalf("relaunch token = %q, %v; want the same", again.Env[envApprovalToken], err)
	}
	info, err := os.Stat(filepath.Join(dataDir, "pi-acp", "session-7", approvalTokenFile))
	if err != nil || info.Size() != 64 {
		t.Fatalf("token file: %v %v", info, err)
	}
	// Another session has its own.
	other, err := toolApprovalToken(dataDir, "session-8")
	if err != nil || other == token {
		t.Fatalf("other session's token = %q, %v", other, err)
	}
}

func TestPiPermissionPolicyAnswersWhatTheModeAllows(t *testing.T) {
	options := []acpsdk.PermissionOption{
		{OptionId: "reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
		{OptionId: "allow", Kind: acpsdk.PermissionOptionKindAllowOnce},
	}
	edit, execute := acpsdk.ToolKindEdit, acpsdk.ToolKindExecute
	for _, test := range []struct {
		mode   ports.PermissionMode
		kind   *acpsdk.ToolKind
		answer bool
	}{
		{mode: ports.PermissionModeBypassPermissions, kind: &execute, answer: true},
		{mode: ports.PermissionModeAuto, kind: &execute, answer: true},
		{mode: ports.PermissionModeAuto, answer: true},
		{mode: ports.PermissionModeAcceptEdits, kind: &edit, answer: true},
		{mode: ports.PermissionModeAcceptEdits, kind: &execute},
		{mode: ports.PermissionModeAcceptEdits},
		{mode: ports.PermissionModeDefault, kind: &edit},
		{mode: ports.PermissionModeDefault, kind: &execute},
	} {
		id, handled := permissionPolicy(test.mode, acpsdk.RequestPermissionRequest{
			ToolCall: acpsdk.ToolCallUpdate{Kind: test.kind}, Options: options,
		})
		if handled != test.answer || (handled && id != "allow") {
			t.Errorf("mode %q kind %v: (%q, %v), want answered %v with allow once", test.mode, test.kind, id, handled, test.answer)
		}
	}
}
