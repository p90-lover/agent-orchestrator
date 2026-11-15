package acp

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Stop pressed again while the agent is still winding the turn down. The cancel
// has already crossed, so the second press has nothing to send, but it was
// answered with an error ("ACP session/cancel is already in progress"). The chat
// service takes any error from Interrupt to mean the stop did not happen and
// dropped the first Stop's queue cutoff over it. A repeat now joins the cancel
// in flight and returns its result.
func TestACPRepeatInterruptJoinsTheCancelAlreadySent(t *testing.T) {
	cancelReceived := make(chan struct{}, 2)
	promptReceived := make(chan struct{}, 1)
	completePrompt := make(chan struct{})
	responseSent := make(chan error, 1)
	driver := New(Config{
		Harness: domain.HarnessOpenCode,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming: true,
			ports.ChatCapabilityInterrupt: true,
		},
		Probe: func(context.Context) error { return nil },
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: "fake"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(delayedCancelSpawn(promptReceived, cancelReceived, completePrompt, responseSent))

	conversation, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conversation.Close()
	// The fake agent holds the prompt until told to answer; a test that fails
	// first must still let it go, or Close waits on it.
	var release sync.Once
	answerPrompt := func() { release.Do(func() { close(completePrompt) }) }
	defer answerPrompt()
	_ = nextEvent(t, conversation.Events()) // controller.ready
	ref, err := conversation.SendTurn(context.Background(), ports.ChatUserMessage{Text: "wait"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conversation.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	select {
	case <-promptReceived:
	case <-time.After(time.Second):
		t.Fatal("provider did not receive ACP prompt request")
	}

	if err := conversation.Interrupt(context.Background(), ref.ProviderTurnID); err != nil {
		t.Fatalf("first Interrupt: %v", err)
	}
	select {
	case <-cancelReceived:
	case <-time.After(time.Second):
		t.Fatal("provider did not receive ACP cancel notification")
	}

	// The turn is still winding down: the provider has the cancel and has not
	// answered the prompt yet.
	if err := conversation.Interrupt(context.Background(), ref.ProviderTurnID); err != nil {
		t.Fatalf("repeat Interrupt while the cancel is in flight: %v", err)
	}

	answerPrompt()
	if err := <-responseSent; err != nil {
		t.Fatalf("provider prompt response: %v", err)
	}
	for {
		event := nextEvent(t, conversation.Events())
		if event.Kind == ports.ChatEventTurnCompleted {
			if event.TurnState != domain.TurnStateInterrupted {
				t.Fatalf("turn state = %q, want interrupted", event.TurnState)
			}
			break
		}
	}
	// The fake agent reads its next request only after answering the prompt, so a
	// second session/cancel from the repeat would show up here.
	select {
	case <-cancelReceived:
		t.Fatal("the repeat sent a second session/cancel")
	case <-time.After(100 * time.Millisecond):
	}
	// Once the turn has ended there is nothing to stop.
	if err := conversation.Interrupt(context.Background(), ref.ProviderTurnID); err != ports.ErrChatNoActiveTurn {
		t.Fatalf("Interrupt after the turn ended = %v, want ErrChatNoActiveTurn", err)
	}
}
