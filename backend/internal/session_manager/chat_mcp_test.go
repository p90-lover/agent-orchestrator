package sessionmanager

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func chatMCPTestServers() []ports.ChatMCPServerConfig {
	return []ports.ChatMCPServerConfig{{
		Name: "coding-tools", Type: "http", URL: "http://127.0.0.1:41234/mcp/ws-1",
		Headers: map[string]string{"Authorization": "Bearer secret-token"},
	}}
}

// The spawn request's tool servers reach the chat controller, and a resume of
// the same session within this daemon's lifetime reattaches them.
func TestChatSpawnPassesMCPServersToTheControllerAndItsResume(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, _, _ := newChatManager(launcher)
	ctx := context.Background()
	servers := chatMCPTestServers()

	rec, _, _, err := mgr.Spawn(ctx, ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat, MCPServers: servers,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(launcher.started) != 1 || !reflect.DeepEqual(launcher.started[0].MCPServers, chatMCPTestServers()) {
		t.Fatalf("controller start mcpServers=%#v", launcher.started)
	}
	// The launch holds its own copy: a caller changing its maps afterwards changes nothing.
	servers[0].Headers["Authorization"] = "changed"
	if launcher.started[0].MCPServers[0].Headers["Authorization"] != "Bearer secret-token" {
		t.Fatal("the launch shares the caller's header map")
	}

	if _, err := mgr.Kill(ctx, rec.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := mgr.RestoreWithMode(ctx, rec.ID); err != nil {
		t.Fatalf("RestoreWithMode: %v", err)
	}
	resumed := launcher.started[len(launcher.started)-1]
	if resumed.ProviderConversationID == "" || !reflect.DeepEqual(resumed.MCPServers, chatMCPTestServers()) {
		t.Fatalf("resume provider=%q mcpServers=%#v", resumed.ProviderConversationID, resumed.MCPServers)
	}
}

func TestChatSpawnWithoutMCPServersStartsWithNone(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, _, _ := newChatManager(launcher)
	if _, _, _, err := mgr.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if len(launcher.started) != 1 || launcher.started[0].MCPServers != nil {
		t.Fatalf("controller start mcpServers=%#v", launcher.started[0].MCPServers)
	}
}

func TestSpawnRefusesMCPServersForTerminalOrInvalidLaunches(t *testing.T) {
	for name, cfg := range map[string]ports.SpawnConfig{
		"terminal": {ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			RequestedMode: domain.SessionModeTUI, MCPServers: chatMCPTestServers()},
		"invalid": {ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
			RequestedMode: domain.SessionModeChat,
			MCPServers:    []ports.ChatMCPServerConfig{{Name: "x", Type: "http", URL: "file:///etc/passwd"}}},
	} {
		launcher := &recordingLauncher{}
		mgr, store, runtime := newChatManager(launcher)
		_, _, _, err := mgr.Spawn(context.Background(), cfg)
		if !errors.Is(err, ports.ErrInvalidMCPServers) {
			t.Fatalf("%s: err=%v, want ErrInvalidMCPServers", name, err)
		}
		if len(launcher.started) != 0 || runtime.created != 0 || len(store.sessions) != 0 {
			t.Fatalf("%s: a refused spawn left state behind (starts=%d runtimes=%d sessions=%d)",
				name, len(launcher.started), runtime.created, len(store.sessions))
		}
	}
}

func TestChatMCPStoreIsBoundToTheHarness(t *testing.T) {
	var store chatMCPStore
	store.set("s-1", domain.HarnessCodex, chatMCPTestServers())
	if got := store.get("s-1", domain.HarnessClaudeCode); got != nil {
		t.Fatalf("another harness received the servers: %#v", got)
	}
	got := store.get("s-1", domain.HarnessCodex)
	got[0].Headers["Authorization"] = "changed"
	if store.get("s-1", domain.HarnessCodex)[0].Headers["Authorization"] != "Bearer secret-token" {
		t.Fatal("get hands out the stored map")
	}
	store.set("s-1", domain.HarnessCodex, nil)
	if store.get("s-1", domain.HarnessCodex) != nil {
		t.Fatal("an empty set clears the entry")
	}
}
