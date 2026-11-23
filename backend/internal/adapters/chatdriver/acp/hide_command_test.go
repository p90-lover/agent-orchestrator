package acp

// From upstream 3d396a0ef, which added these to driver_test.go. The fork's
// driver_test.go has diverged too far for that hunk to apply, so they live here.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestACPDriverHidesCommandsTheBindingNames(t *testing.T) {
	agent := &fakeAgent{}
	driver := New(Config{
		Harness:      domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
		HideCommand:  HideCommands("model", "compact"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()

	if err := agent.conn.SessionUpdate(context.Background(), acpsdk.SessionNotification{
		SessionId: acpsdk.SessionId(conv.ProviderConversationID()),
		Update: acpsdk.SessionUpdate{AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
			AvailableCommands: []acpsdk.AvailableCommand{
				{Name: "goal", Description: "Keep working until a condition is met"},
				{Name: "model", Description: "Set the AI model"},
				{Name: "compact", Description: "Summarize the conversation"},
			},
		}},
	}); err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}

	skills := awaitSkillCount(t, conv.(ports.ChatSkillLister), 1)
	if skills[0].Name != "goal" {
		t.Fatalf("skills = %#v, want only goal", skills)
	}
	// Hiding is presentation only: the agent's compaction stays usable.
	if !conv.Capabilities().Has(ports.ChatCapabilityCompaction) {
		t.Fatal("hiding compact from the menu also disabled compaction")
	}
}

// Commands can arrive during session/new, before start() derives compaction.
// Hiding compact from the menu must not turn compaction off on that path either.
func TestACPEarlyHiddenCompactKeepsCompactionInStart(t *testing.T) {
	agent := &fakeAgent{
		newSessionUpdates: []acpsdk.SessionUpdate{{
			AvailableCommandsUpdate: &acpsdk.SessionAvailableCommandsUpdate{
				AvailableCommands: []acpsdk.AvailableCommand{
					{Name: "compact", Description: "Compact history"},
					{Name: "goal", Description: "Keep working until a condition is met"},
				},
			},
		}},
	}
	driver := New(Config{
		Harness:      domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
		HideCommand:  HideCommands("compact"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	opened, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer opened.Close()

	if !opened.Capabilities().Has(ports.ChatCapabilityCompaction) {
		t.Fatal("a compact hidden from the menu before start() disabled compaction")
	}
	skills := awaitSkillCount(t, opened.(ports.ChatSkillLister), 1)
	if skills[0].Name != "goal" {
		t.Fatalf("skills = %#v, want only goal", skills)
	}
}
