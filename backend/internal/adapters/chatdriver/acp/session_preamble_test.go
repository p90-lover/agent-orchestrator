package acp

// The fork never took upstream's Command Code ACP driver (37d1ce7f6), which is
// where Config.SessionPreamble and its tests first appeared. The Antigravity
// binding depends on that delivery channel, so the channel was ported with it
// and these tests come from the same upstream commit, pointed at Agy instead of
// Command Code.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// standingInstructions is the fixed preamble text used by the preamble tests.
const standingInstructions = "AO standing instructions for this session"

// runOneTurn sends a single text turn and returns the prompt the provider saw.
// The caller must already have drained the controller.ready event emitted at
// session setup.
func runOneTurn(t *testing.T, agent *fakeAgent, conv ports.ChatConversation, text string) []acpsdk.ContentBlock {
	t.Helper()
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: text, ClientMessageID: "ao-client-message-preamble",
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	for {
		if event := nextEvent(t, conv.Events()); event.Kind == ports.ChatEventTurnCompleted {
			break
		}
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return append([]acpsdk.ContentBlock(nil), agent.promptParams.Prompt...)
}

// preambleText reads AO's standing instructions back out of a native prompt the
// way the agent does: the resource block is what reaches the model.
func preambleText(prompt []acpsdk.ContentBlock) string {
	if len(prompt) == 0 || prompt[0].Resource == nil {
		return ""
	}
	contents := prompt[0].Resource.Resource.TextResourceContents
	if contents == nil {
		return ""
	}
	return contents.Text
}

// preambleConfig is the binding shape under test: an agent with no
// system-prompt launch flag that ignores session/new _meta and receives AO's
// standing context only on the first prompt.
func preambleConfig() Config {
	return Config{
		Harness:      domain.HarnessAgy,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
		SessionPreamble: func(cfg LaunchConfig) []acpsdk.ContentBlock {
			return []acpsdk.ContentBlock{acpsdk.ResourceBlock(acpsdk.EmbeddedResourceResource{
				TextResourceContents: &acpsdk.TextResourceContents{
					Uri:  "ao://agent-orchestrator/standing-instructions",
					Text: cfg.SystemPrompt,
				},
			})}
		},
	}
}

// TestACPDriverDeliversSessionPreambleOnStartAndResume covers the channel AO
// uses for agents that accept no system-prompt launch flag and ignore
// session/new _meta: a block leading the first prompt. A resumed session with
// no prior turns must receive the recomputed instructions.
func TestACPDriverDeliversSessionPreambleOnStartAndResume(t *testing.T) {
	newDriver := func(agent *fakeAgent) *Driver {
		driver := New(preambleConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		driver.useTestProcess(fakeSpawn(agent))
		return driver
	}

	t.Run("start", func(t *testing.T) {
		agent := &fakeAgent{promptNoPermission: true}
		conv, err := newDriver(agent).Start(context.Background(), ports.ChatStartConfig{
			WorkspacePath: t.TempDir(), SystemPrompt: standingInstructions,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer conv.Close()
		_ = nextEvent(t, conv.Events()) // controller.ready
		prompt := runOneTurn(t, agent, conv, "do the task")
		if got := preambleText(prompt); got != standingInstructions {
			t.Fatalf("first prompt standing context = %q, want %q (prompt %#v)", got, standingInstructions, prompt)
		}
		if len(prompt) < 2 || prompt[1].Text == nil || prompt[1].Text.Text != "do the task" {
			t.Fatalf("standing context did not lead the user text: %#v", prompt)
		}
	})

	t.Run("resume", func(t *testing.T) {
		agent := &fakeAgent{promptNoPermission: true}
		conv, err := newDriver(agent).Resume(context.Background(), ports.ChatResumeConfig{
			SessionID:              "worker-1",
			ProviderConversationID: "provider-session-1",
			WorkspacePath:          t.TempDir(),
			SystemPrompt:           standingInstructions,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		defer conv.Close()
		_ = nextEvent(t, conv.Events()) // controller.ready
		prompt := runOneTurn(t, agent, conv, "continue the task")
		if got := preambleText(prompt); got != standingInstructions {
			t.Fatalf("resumed first prompt standing context = %q, want %q (prompt %#v)", got, standingInstructions, prompt)
		}
	})

	t.Run("resume with durable turns", func(t *testing.T) {
		agent := &fakeAgent{promptNoPermission: true}
		conv, err := newDriver(agent).Resume(context.Background(), ports.ChatResumeConfig{
			SessionID:              "worker-1",
			ProviderConversationID: "provider-session-1",
			WorkspacePath:          t.TempDir(),
			SystemPrompt:           standingInstructions,
			HasPriorTurns:          true,
		})
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		defer conv.Close()
		_ = nextEvent(t, conv.Events()) // controller.ready
		if got := preambleText(runOneTurn(t, agent, conv, "continue the task")); got != "" {
			t.Fatalf("resumed conversation with prior turns repeated standing context %q", got)
		}
	})
}

// Standing context is process input, so it is delivered once per session rather
// than repeated on every turn, which would grow the transcript and re-state the
// same rules mid-conversation.
func TestACPDriverDeliversSessionPreambleOncePerSession(t *testing.T) {
	agent := &fakeAgent{promptNoPermission: true}
	driver := New(preambleConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))

	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: t.TempDir(), SystemPrompt: standingInstructions,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()
	_ = nextEvent(t, conv.Events()) // controller.ready
	if got := preambleText(runOneTurn(t, agent, conv, "first")); got != standingInstructions {
		t.Fatalf("first prompt standing context = %q", got)
	}
	agent.mu.Lock()
	agent.promptParams = acpsdk.PromptRequest{}
	agent.mu.Unlock()
	if got := preambleText(runOneTurn(t, agent, conv, "second")); got != "" {
		t.Fatalf("second prompt repeated standing context %q; want it delivered once per session", got)
	}
}

// A first turn that never reaches the provider must not spend the standing
// context. Both paths here fail in SendTurn, after the prompt is built but
// before any session/prompt is sent.
func TestACPDriverPreservesSessionPreambleWhenFirstTurnNeverStarts(t *testing.T) {
	newConversation := func(t *testing.T, agent *fakeAgent) ports.ChatConversation {
		t.Helper()
		driver := New(preambleConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		driver.useTestProcess(fakeSpawn(agent))
		conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
			WorkspacePath: t.TempDir(), SystemPrompt: standingInstructions,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { conv.Close() })
		_ = nextEvent(t, conv.Events()) // controller.ready
		return conv
	}

	t.Run("rejected first-turn settings", func(t *testing.T) {
		agent := &fakeAgent{promptNoPermission: true}
		reject := false
		driverConfig := preambleConfig()
		driverConfig.ValidateTurnSettings = func(_ ports.PermissionMode, settings ports.ChatTurnSettings) error {
			if reject && settings.Model == "rejected-model" {
				return errors.New("settings rejected")
			}
			return nil
		}
		driver := New(driverConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
		driver.useTestProcess(fakeSpawn(agent))
		conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
			WorkspacePath: t.TempDir(), SystemPrompt: standingInstructions,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer conv.Close()
		_ = nextEvent(t, conv.Events()) // controller.ready

		reject = true
		if _, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
			Text:     "do the task",
			Settings: ports.ChatTurnSettings{Model: "rejected-model"},
		}); err == nil {
			t.Fatal("SendTurn with rejected settings succeeded; want an error")
		}
		agent.mu.Lock()
		reached := agent.promptParams.Prompt != nil
		agent.mu.Unlock()
		if reached {
			t.Fatal("a turn whose settings were rejected still reached the provider")
		}

		reject = false
		if got := preambleText(runOneTurn(t, agent, conv, "retry the task")); got != standingInstructions {
			t.Fatalf("retry standing context = %q, want %q; a rejected turn must not spend it", got, standingInstructions)
		}
	})

	t.Run("discarded prepared turn", func(t *testing.T) {
		agent := &fakeAgent{promptNoPermission: true}
		conv := newConversation(t, agent)

		ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
			Text: "abandoned task", ClientMessageID: "ao-client-message-discarded",
		})
		if err != nil {
			t.Fatalf("SendTurn: %v", err)
		}
		conv.(ports.ChatDeferredTurnStarter).DiscardDeferredTurn(ref.ProviderTurnID)
		agent.mu.Lock()
		reached := agent.promptParams.Prompt != nil
		agent.mu.Unlock()
		if reached {
			t.Fatal("a discarded turn still reached the provider")
		}

		if got := preambleText(runOneTurn(t, agent, conv, "the real task")); got != standingInstructions {
			t.Fatalf("next prompt standing context = %q, want %q; a discarded turn must not spend it", got, standingInstructions)
		}
	})
}
