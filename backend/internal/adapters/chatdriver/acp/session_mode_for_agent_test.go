package acp

import (
	"context"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SessionModeForAgent lets a binding whose ACP servers name their modes
// differently (Antigravity's separate server and `agy acp`) pick the mapping
// from the handshake. A mapping it returns replaces SessionMode; nil keeps it.
func TestACPDriverSessionModeForAgentOverridesSessionMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
		want     string
	}{
		{name: "agent mapping", override: true, want: "accept-edits"},
		{name: "binding default", override: false, want: "yolo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{}
			var sawHandshake bool
			driver := New(Config{
				Harness: domain.HarnessAgy,
				Probe:   func(context.Context) error { return nil },
				Launch:  func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
				SessionMode: func(ports.PermissionMode) string {
					return "yolo"
				},
				SessionModeForAgent: func(init acpsdk.InitializeResponse) func(ports.PermissionMode) string {
					sawHandshake = init.ProtocolVersion == acpsdk.ProtocolVersionNumber
					if !tc.override {
						return nil
					}
					return func(ports.PermissionMode) string { return "accept-edits" }
				},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			driver.useTestProcess(fakeSpawn(agent))

			conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
				WorkspacePath: t.TempDir(),
				Permissions:   ports.PermissionModeAuto,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer conv.Close()

			if !sawHandshake {
				t.Fatal("SessionModeForAgent did not receive the initialize response")
			}
			agent.mu.Lock()
			defer agent.mu.Unlock()
			if agent.mode != tc.want {
				t.Fatalf("mode = %q, want %q", agent.mode, tc.want)
			}
		})
	}
}
