package acp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestIsACPSessionGone(t *testing.T) {
	const session = "claude-session-1"
	notFound := func(data any) error {
		return &acpsdk.RequestError{Code: -32002, Message: "Resource not found", Data: data}
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"own session", notFound(map[string]any{"sessionId": session}), true},
		{"own session, wrapped", errors.Join(errors.New("set ACP session option"), notFound(map[string]any{"sessionId": session})), true},
		{"another session", notFound(map[string]any{"sessionId": "claude-session-2"}), false},
		{"a resource", notFound(map[string]any{"uri": "file:///missing.txt"}), false},
		{"no data", notFound(nil), false},
		{"internal error naming the session", &acpsdk.RequestError{Code: -32603, Message: "Internal error", Data: map[string]any{"sessionId": session}}, false},
		{"plain error", errors.New("Resource not found"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isACPSessionGone(tc.err, session); got != tc.want {
				t.Fatalf("isACPSessionGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	if isACPSessionGone(notFound(map[string]any{"sessionId": ""}), "") {
		t.Fatal("a conversation with no session cannot lose it")
	}
}

func sessionReopenTestDriver(agent *fakeAgent) *Driver {
	agent.promptNoPermission = true
	driver := New(Config{
		Harness:      domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{ports.ChatCapabilityStreaming: true},
		Probe:        func(context.Context) error { return nil },
		Launch:       func(context.Context, LaunchConfig) (Launch, error) { return Launch{Command: "fake"}, nil },
		SessionMeta: func(LaunchConfig) map[string]any {
			return map[string]any{"systemPrompt": map[string]any{"append": "standing instructions"}}
		},
		SessionMode: func(mode ports.PermissionMode) string {
			if mode == "" {
				return ""
			}
			return "default"
		},
		SessionOptions: func(settings ports.ChatTurnSettings) []SessionOption {
			if settings.Model == "" {
				return nil
			}
			return []SessionOption{{ID: "model", Value: settings.Model}}
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(fakeSpawn(agent))
	return driver
}

// drainControllerStates returns the controller states emitted so far, in order.
func drainControllerStates(events <-chan ports.ChatEvent) []ports.ChatControllerState {
	var states []ports.ChatControllerState
	for {
		select {
		case event := <-events:
			if event.Kind == ports.ChatEventControllerState {
				states = append(states, event.ControllerState)
			}
		default:
			return states
		}
	}
}

// awaitTurnCompleted reads events until the turn completes and returns that
// event and the controller states that follow it.
func awaitTurnCompleted(t *testing.T, events <-chan ports.ChatEvent, turnID string) (ports.ChatEvent, ports.ChatControllerState) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var completed *ports.ChatEvent
	for {
		select {
		case event := <-events:
			if event.Kind == ports.ChatEventTurnCompleted && event.ProviderTurnID == turnID {
				completed = &event
				continue
			}
			if completed != nil && event.Kind == ports.ChatEventControllerState {
				return *completed, event.ControllerState
			}
		case <-deadline:
			t.Fatalf("timed out waiting for turn %s to complete", turnID)
		}
	}
}

func TestACPDriverReopensDroppedSessionBeforeTurn(t *testing.T) {
	agent := &fakeAgent{}
	driver := sessionReopenTestDriver(agent)
	workspace := t.TempDir()
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: workspace, Model: "opus", Permissions: ports.PermissionModeDefault,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()
	drainControllerStates(conv.Events())

	// The Claude process behind the session died and the adapter evicted it.
	agent.mu.Lock()
	agent.dropped = "claude-session-1"
	agent.options = nil
	agent.mu.Unlock()

	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text:     "follow-up",
		Settings: ports.ChatTurnSettings{Model: "opus", Approval: ports.PermissionModeDefault},
	})
	if err != nil {
		t.Fatalf("SendTurn on a dropped session: %v", err)
	}

	agent.mu.Lock()
	resumeCalls, resumed, model, mode := agent.resumeCalls, agent.resumeParams, agent.options["model"], agent.mode
	agent.mu.Unlock()
	if resumeCalls != 1 {
		t.Fatalf("session/resume calls = %d, want 1", resumeCalls)
	}
	if resumed.SessionId != "claude-session-1" || resumed.Cwd != workspace {
		t.Fatalf("session/resume = %#v, want the conversation's own session in its workspace", resumed)
	}
	if prompt, _ := resumed.Meta["systemPrompt"].(map[string]any); prompt["append"] != "standing instructions" {
		t.Fatalf("session/resume meta = %#v, want the session's standing instructions again", resumed.Meta)
	}
	if model != "opus" || mode != "default" {
		t.Fatalf("settings after reopen: model %q mode %q, want them applied to the reopened session", model, mode)
	}
	if states := drainControllerStates(conv.Events()); len(states) != 2 ||
		states[0] != ports.ChatControllerRecovering || states[1] != ports.ChatControllerReady {
		t.Fatalf("controller states = %v, want [recovering ready]", states)
	}

	deferred := conv.(ports.ChatDeferredTurnStarter)
	if err := deferred.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	completed, idle := awaitTurnCompleted(t, conv.Events(), ref.ProviderTurnID)
	if completed.TurnState != domain.TurnStateCompleted || completed.Err != nil {
		t.Fatalf("turn after reopen = %#v, want completed", completed)
	}
	if idle != ports.ChatControllerReady {
		t.Fatalf("controller after the turn = %q, want ready", idle)
	}
	agent.mu.Lock()
	promptSession := agent.promptParams.SessionId
	agent.mu.Unlock()
	if promptSession != "claude-session-1" {
		t.Fatalf("prompt went to session %q, want the reopened claude-session-1", promptSession)
	}
}

func TestACPDriverDroppedSessionReopenFailureIsResumeFailed(t *testing.T) {
	agent := &fakeAgent{}
	driver := sessionReopenTestDriver(agent)
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: t.TempDir(), Permissions: ports.PermissionModeDefault,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()
	drainControllerStates(conv.Events())

	agent.mu.Lock()
	agent.dropped = "claude-session-1"
	agent.resumeErr = errors.New("no conversation found with that session id")
	agent.mu.Unlock()

	msg := ports.ChatUserMessage{Text: "follow-up", Settings: ports.ChatTurnSettings{Approval: ports.PermissionModeDefault}}
	_, err = conv.SendTurn(context.Background(), msg)
	if !errors.Is(err, ports.ErrChatResumeFailed) {
		t.Fatalf("SendTurn when the reopen fails: err = %v, want ErrChatResumeFailed", err)
	}
	if states := drainControllerStates(conv.Events()); len(states) != 1 || states[0] != ports.ChatControllerRecovering {
		t.Fatalf("controller states = %v, want [recovering]", states)
	}

	// The next turn tries again, before it sends anything to the dead session.
	agent.mu.Lock()
	agent.resumeErr = nil
	droppedBefore := agent.droppedCalls
	agent.mu.Unlock()
	ref, err := conv.SendTurn(context.Background(), msg)
	if err != nil {
		t.Fatalf("SendTurn once the reopen works: %v", err)
	}
	conv.(ports.ChatDeferredTurnStarter).DiscardDeferredTurn(ref.ProviderTurnID)
	agent.mu.Lock()
	resumeCalls, droppedCalls, mode := agent.resumeCalls, agent.droppedCalls, agent.mode
	agent.mu.Unlock()
	if resumeCalls != 2 {
		t.Fatalf("session/resume calls = %d, want 2", resumeCalls)
	}
	if droppedCalls != droppedBefore {
		t.Fatalf("requests sent to the dead session = %d, want none", droppedCalls-droppedBefore)
	}
	if mode != "default" {
		t.Fatalf("mode after reopen = %q, want the turn's mode applied", mode)
	}
	if states := drainControllerStates(conv.Events()); len(states) != 1 || states[0] != ports.ChatControllerReady {
		t.Fatalf("controller states = %v, want [ready]", states)
	}
}

func TestACPDriverDroppedSessionWithoutResumeIsResumeFailed(t *testing.T) {
	agent := &fakeAgent{capabilities: &acpsdk.AgentCapabilities{}}
	driver := sessionReopenTestDriver(agent)
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{
		WorkspacePath: t.TempDir(), Permissions: ports.PermissionModeDefault,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()

	agent.mu.Lock()
	agent.dropped = "claude-session-1"
	agent.mu.Unlock()
	_, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{
		Text: "follow-up", Settings: ports.ChatTurnSettings{Approval: ports.PermissionModeDefault},
	})
	if !errors.Is(err, ports.ErrChatResumeFailed) {
		t.Fatalf("SendTurn on a dropped session the agent cannot resume: err = %v, want ErrChatResumeFailed", err)
	}
	agent.mu.Lock()
	resumeCalls := agent.resumeCalls
	agent.mu.Unlock()
	if resumeCalls != 0 {
		t.Fatalf("session/resume calls = %d, want none from an agent that does not advertise it", resumeCalls)
	}
}

func TestACPDriverOtherResourceNotFoundIsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name string
		data any
	}{
		{"another session", map[string]any{"sessionId": "claude-session-2"}},
		{"a resource", map[string]any{"uri": "file:///missing.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{}
			driver := sessionReopenTestDriver(agent)
			conv, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer conv.Close()
			drainControllerStates(conv.Events())

			agent.mu.Lock()
			agent.configErr = &acpsdk.RequestError{Code: -32002, Message: "Resource not found", Data: tc.data}
			agent.mu.Unlock()
			_, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{
				Text: "follow-up", Settings: ports.ChatTurnSettings{Model: "opus"},
			})
			var requestErr *acpsdk.RequestError
			if !errors.As(err, &requestErr) || requestErr.Code != -32002 || errors.Is(err, ports.ErrChatResumeFailed) {
				t.Fatalf("SendTurn: err = %v, want the agent's own -32002 unchanged", err)
			}
			agent.mu.Lock()
			resumeCalls := agent.resumeCalls
			agent.mu.Unlock()
			if resumeCalls != 0 {
				t.Fatalf("session/resume calls = %d, want none", resumeCalls)
			}
			if states := drainControllerStates(conv.Events()); len(states) != 0 {
				t.Fatalf("controller states = %v, want none", states)
			}
		})
	}
}

func TestACPDriverPromptOnDroppedSessionLeavesRecovering(t *testing.T) {
	agent := &fakeAgent{}
	driver := sessionReopenTestDriver(agent)
	conv, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conv.Close()
	drainControllerStates(conv.Events())
	deferred := conv.(ports.ChatDeferredTurnStarter)

	// No settings: nothing reaches the agent before the prompt itself.
	ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "first"})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	agent.mu.Lock()
	agent.dropped = "claude-session-1"
	agent.mu.Unlock()
	if err := deferred.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	completed, idle := awaitTurnCompleted(t, conv.Events(), ref.ProviderTurnID)
	if completed.TurnState != domain.TurnStateFailed {
		t.Fatalf("prompt on a dropped session = %#v, want a failed turn", completed)
	}
	if idle != ports.ChatControllerRecovering {
		t.Fatalf("controller after the failed prompt = %q, want recovering", idle)
	}

	// The follow-up reopens the session before it sends anything.
	agent.mu.Lock()
	droppedBefore := agent.droppedCalls
	agent.mu.Unlock()
	ref, err = conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "follow-up"})
	if err != nil {
		t.Fatalf("SendTurn after the dropped prompt: %v", err)
	}
	agent.mu.Lock()
	resumeCalls, droppedCalls := agent.resumeCalls, agent.droppedCalls
	agent.mu.Unlock()
	if resumeCalls != 1 || droppedCalls != droppedBefore {
		t.Fatalf("session/resume calls = %d, requests to the dead session = %d; want 1 and none",
			resumeCalls, droppedCalls-droppedBefore)
	}
	if err := deferred.StartDeferredTurn(ref.ProviderTurnID); err != nil {
		t.Fatalf("StartDeferredTurn: %v", err)
	}
	completed, idle = awaitTurnCompleted(t, conv.Events(), ref.ProviderTurnID)
	if completed.TurnState != domain.TurnStateCompleted || idle != ports.ChatControllerReady {
		t.Fatalf("follow-up = %#v then %q, want completed then ready", completed, idle)
	}
}
