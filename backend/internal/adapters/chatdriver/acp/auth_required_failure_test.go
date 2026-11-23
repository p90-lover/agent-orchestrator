package acp

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

// A binding whose agent reports provider refusals as auth-required gets to name
// the real failure: the turn then carries the provider's words, raises no
// reauthentication demand and leaves the auth cache alone. Returning nil keeps
// today's auth failure.
func TestACPDriverAuthRequiredFailureReplacesTheAuthError(t *testing.T) {
	refusal := ports.NewChatProviderFailure("API Error: 403 Access requires an eligible paid billing account in a supported region.", "", nil)
	for _, tc := range []struct {
		name        string
		replacement error
		wantAuth    bool
	}{
		{name: "provider refusal", replacement: refusal, wantAuth: false},
		{name: "real sign-in failure", replacement: nil, wantAuth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			var got []AuthRequiredContext
			authRejected := 0
			agent := &fakeAgent{promptErr: acpsdk.NewAuthRequired(nil)}
			driver := New(Config{
				Harness:      domain.HarnessClaudeCode,
				Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
				Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
				OnAuthRejected: func() {
					authRejected++
				},
				AuthRequiredFailure: func(pc AuthRequiredContext) error {
					got = append(got, pc)
					return tc.replacement
				},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			driver.useTestProcess(fakeSpawn(agent))
			opened, err := driver.Start(context.Background(), ports.ChatStartConfig{
				WorkspacePath: workspace,
				Env:           map[string]string{"CLAUDE_CONFIG_DIR": "claude-config"},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			_ = nextEvent(t, opened.Events())
			ref, err := opened.SendTurn(context.Background(), ports.ChatUserMessage{Text: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
				t.Fatal(err)
			}
			for {
				event := nextEvent(t, opened.Events())
				if event.Kind != ports.ChatEventTurnCompleted {
					continue
				}
				if event.TurnState != domain.TurnStateFailed {
					t.Fatalf("turn state = %q, want failed", event.TurnState)
				}
				if errors.Is(event.Err, ports.ErrChatAuthRequired) != tc.wantAuth {
					t.Fatalf("completion error = %v; auth-required = %v, want %v", event.Err, !tc.wantAuth, tc.wantAuth)
				}
				if !tc.wantAuth && event.Err.Error() != refusal.Error() {
					t.Fatalf("completion error = %q, want the provider's words %q", event.Err, refusal)
				}
				break
			}
			if len(got) != 1 {
				t.Fatalf("hook ran %d times, want once", len(got))
			}
			if got[0].SessionID != opened.ProviderConversationID() || got[0].WorkspacePath != workspace ||
				got[0].Env["CLAUDE_CONFIG_DIR"] != "claude-config" {
				t.Fatalf("hook context = %#v, want session %q in %q", got[0], opened.ProviderConversationID(), workspace)
			}
			wantRejected := 0
			if tc.wantAuth {
				wantRejected = 1
			}
			if authRejected != wantRejected {
				t.Fatalf("auth cache invalidations = %d, want %d", authRejected, wantRejected)
			}
		})
	}
}

// Only ACP's auth-required error asks the binding: any other failed prompt is
// reported as it was.
func TestAuthRequiredFailureIsNotAskedForOtherErrors(t *testing.T) {
	asked := 0
	conv := &conversation{
		sessionID: "session-1", activeTurn: "turn-1",
		events: make(chan ports.ChatEvent, 16), log: slog.New(slog.DiscardHandler),
		authRequiredFailure: func(string) error { asked++; return errors.New("must not be used") },
	}
	conv.finishPrompt("turn-1", acpsdk.PromptResponse{}, &acpsdk.RequestError{Code: -32603, Message: "Internal error"})
	close(conv.events)
	for event := range conv.events {
		if event.Kind == ports.ChatEventTurnCompleted && (event.Err == nil || event.Err.Error() == "must not be used") {
			t.Fatalf("completion error = %v, want the internal error", event.Err)
		}
	}
	if asked != 0 {
		t.Fatalf("hook asked %d times for a non-auth error", asked)
	}
}
