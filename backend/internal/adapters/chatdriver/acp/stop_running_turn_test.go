package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A turn that was running when AO stopped its agent ends as interrupted, with no
// error. Seen 2026-11-16: a hung turn's stop shut the agent's host down, and the
// chat showed the step failed with
// `ACP session/prompt: {"code":-32800,"message":"Request cancelled","data":{"error":"context canceled"}}`.
// The pinned SDK answers a caller whose context was cancelled with that JSON-RPC
// error, which is not context.Canceled to errors.Is, so finishPrompt called the
// turn failed.
//
// The turn is also reported before the provider goes. The SDK writes a
// session/cancel to the agent before the cancelled prompt returns, so the stop,
// which did not wait, could close the event stream first: the turn's end was then
// dropped and the turn stayed "running" (20 of 100 runs of this test).
func TestACPStoppingTheAgentInterruptsItsRunningTurn(t *testing.T) {
	promptReceived := make(chan struct{}, 1)
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
	driver.useTestProcess(promptHoldingSpawn(promptReceived))

	conversation, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conversation.Close()
	_ = nextEvent(t, conversation.Events()) // controller.ready
	ref, err := conversation.SendTurn(context.Background(), ports.ChatUserMessage{Text: "work"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if err := conversation.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	<-promptReceived

	stopped := make(chan error, 1)
	go func() { stopped <- conversation.(ports.ChatProviderTerminator).Terminate() }()

	for {
		event := nextEvent(t, conversation.Events())
		if event.Kind != ports.ChatEventTurnCompleted {
			continue
		}
		if event.ProviderTurnID != ref.ProviderTurnID {
			t.Fatalf("completed turn = %q, want %q", event.ProviderTurnID, ref.ProviderTurnID)
		}
		if event.TurnState != domain.TurnStateInterrupted || event.Err != nil {
			t.Fatalf("turn ended %q with error %v, want interrupted with no error", event.TurnState, event.Err)
		}
		break
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

// Only a prompt whose own context AO cancelled is read as interrupted: the same
// JSON-RPC code from the agent, for a prompt AO did not cancel, stays a failure.
func TestLocallyCancelledReadsOnlyTheTurnsOwnContext(t *testing.T) {
	cancelled := acpsdk.NewRequestCancelled(map[string]any{"error": "context canceled"})

	live := context.Background()
	if got := locallyCancelled(live, cancelled); got != error(cancelled) || errors.Is(got, context.Canceled) {
		t.Fatalf("a live turn's -32800 = %v, want the agent's error unchanged", got)
	}
	if got := locallyCancelled(live, nil); got != nil {
		t.Fatalf("a live turn without an error = %v, want nil", got)
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	got := locallyCancelled(ended, cancelled)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("a cancelled turn's error = %v, want context.Canceled", got)
	}
	var request *acpsdk.RequestError
	if !errors.As(got, &request) || request.Code != -32800 || !strings.Contains(got.Error(), "Request cancelled") {
		t.Fatalf("a cancelled turn's error = %v, want it to keep the agent's answer", got)
	}
	// A prompt that had already answered when the stop came keeps its result.
	if got := locallyCancelled(ended, nil); got != nil {
		t.Fatalf("an answered prompt on a cancelled turn = %v, want nil", got)
	}
}

// promptHoldingSpawn is an agent that takes a prompt and never answers it. It
// keeps reading what follows (the cancel notices of a stop) and answers
// session/close, as an agent that is busy but alive does.
func promptHoldingSpawn(promptReceived chan<- struct{}) spawnFunc {
	return func(Launch, string) (*process, error) {
		clientToAgentR, clientToAgentW := io.Pipe()
		agentToClientR, agentToClientW := io.Pipe()
		go func() {
			decoder := json.NewDecoder(clientToAgentR)
			encoder := json.NewEncoder(agentToClientW)
			for {
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := decoder.Decode(&request); err != nil {
					return
				}
				switch request.Method {
				case "initialize":
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"result": map[string]any{
							"protocolVersion":   acpsdk.ProtocolVersionNumber,
							"agentCapabilities": map[string]any{},
							"authMethods":       []any{},
						},
					})
				case "session/new":
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID,
						"result": map[string]any{"sessionId": "held-session"},
					})
				case "session/prompt":
					select {
					case promptReceived <- struct{}{}:
					default:
					}
				case "session/close":
					_ = encoder.Encode(map[string]any{
						"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{},
					})
				}
			}
		}()
		var once sync.Once
		return &process{
			stdin: clientToAgentW, stdout: agentToClientR,
			stop: func() error {
				once.Do(func() {
					_ = clientToAgentW.Close()
					_ = clientToAgentR.Close()
					_ = agentToClientW.Close()
					_ = agentToClientR.Close()
				})
				return nil
			},
		}, nil
	}
}
