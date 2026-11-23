package acp

// From upstream 7c1b39c13, which added these to driver_test.go. The fork's
// driver_test.go has diverged too far for that hunk to apply, so they live here.

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestHistoricalUserContentDropsPlainStandingInstructionsJoinedToTheFirstMessage(t *testing.T) {
	standing := PlainStandingInstructionsBlock("AO standing instructions")
	// Antigravity replays the preamble and the first prompt as one message.
	joined := acpsdk.TextBlock(standing.Text.Text + "\n\nFix the failing test")
	if got := historicalUserContent(joined); got != "Fix the failing test" {
		t.Fatalf("replayed first message = %q, want only what the user typed", got)
	}
	if got := historicalUserContent(standing); got != "" {
		t.Fatalf("replayed preamble alone = %q, want empty", got)
	}
	quoted := acpsdk.TextBlock("Explain </ao_standing_instructions> in the docs")
	if got := historicalUserContent(quoted); got != quoted.Text.Text {
		t.Fatalf("message quoting the tag = %q, want it unchanged", got)
	}
}

// Antigravity 1.5.14 replays the first prompt as two id-less user chunks: the
// standing-instructions preamble, then what the user typed. The merged first
// message must be only the user's text, or the ao_high_water checkpoint
// never matches.
func TestACPHistoryReplayDropsPlainStandingInstructionsChunkBeforeTheFirstPrompt(t *testing.T) {
	agent := &fakeAgent{
		capabilities: &acpsdk.AgentCapabilities{LoadSession: true},
		loadUpdates: []acpsdk.SessionUpdate{
			acpsdk.UpdateUserMessage(PlainStandingInstructionsBlock("AO standing instructions")),
			acpsdk.UpdateUserMessageText("Fix the failing test"),
			acpsdk.UpdateAgentMessageText("Fixed."),
		},
	}
	driver := New(Config{
		Harness:      domain.HarnessAgy,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true, ports.ChatCapabilityResume: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))
	conv, err := driver.Resume(context.Background(), ports.ChatResumeConfig{
		SessionID: "sess1", WorkspacePath: t.TempDir(), ProviderConversationID: "provider-session-1",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer conv.Close()
	events, err := conv.(ports.ChatHistoryReader).ReadHistory(context.Background())
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	var users []string
	for _, event := range events {
		if event.Kind == ports.ChatEventUserMessageCompleted {
			users = append(users, event.Text)
		}
	}
	if want := []string{"Fix the failing test"}; !reflect.DeepEqual(users, want) {
		t.Fatalf("replayed user messages = %q, want %q", users, want)
	}
}
