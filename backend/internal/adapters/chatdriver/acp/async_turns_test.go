package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const testAutonomousTurn = "autonomous-turn-7f0c"

// A turn Claude Code starts by itself (here: another local Claude session sent
// it a message) is a turn of its own in AO, and its permission request is filed
// under it. Before, AO did not declare asyncTasks, the turn's output had no turn,
// and its first permission request was never shown: the chat read Done while the
// agent waited for hours (2026-11-09).
func TestACPAutonomousTurnIsAdoptedWithItsApproval(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	defer conversation.Close()

	// Back to back, as the agent writes them: the SDK runs the request on its own
	// goroutine, so it must still be filed under the turn the update opens.
	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	agent.send(map[string]any{
		"jsonrpc": "2.0", "id": 41, "method": "session/request_permission",
		"params": map[string]any{
			"sessionId": scriptedSessionID,
			"toolCall":  map[string]any{"toolCallId": "tool-1", "title": "Run git push", "kind": "execute"},
			"options": []any{
				map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
			},
		},
	})

	started := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnStarted)
	if started.ProviderTurnID != testAutonomousTurn {
		t.Fatalf("started turn = %q, want %q", started.ProviderTurnID, testAutonomousTurn)
	}
	busy := nextEventOfKind(t, conversation.Events(), ports.ChatEventControllerState)
	if busy.ControllerState != ports.ChatControllerBusy {
		t.Fatalf("controller state = %q, want busy", busy.ControllerState)
	}
	approval := nextEventOfKind(t, conversation.Events(), ports.ChatEventApprovalRequested)
	if approval.ProviderTurnID != testAutonomousTurn {
		t.Fatalf("approval filed under turn %q, want %q", approval.ProviderTurnID, testAutonomousTurn)
	}
	if err := conversation.ResolveRequest(context.Background(), approval.RequestID, ports.ChatDecision{ID: "allow"}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	answer := agent.nextResponse(t, "41")
	if !strings.Contains(string(answer), `"allow"`) {
		t.Fatalf("permission answer = %s, want allow", answer)
	}

	agent.send(sessionUpdate(map[string]any{
		"sessionUpdate": "agent_message_chunk", "messageId": "reply-1",
		"content": map[string]any{"type": "text", "text": "Pushed."},
	}))
	agent.send(asyncTaskState(testAutonomousTurn, "completed", ""))

	delta := nextEventOfKind(t, conversation.Events(), ports.ChatEventMessageDelta)
	if delta.ProviderTurnID != testAutonomousTurn {
		t.Fatalf("reply filed under turn %q, want %q", delta.ProviderTurnID, testAutonomousTurn)
	}
	completed := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnCompleted)
	if completed.ProviderTurnID != testAutonomousTurn || completed.TurnState != domain.TurnStateCompleted || completed.Err != nil {
		t.Fatalf("turn ended %q %q (%v), want %q completed", completed.ProviderTurnID, completed.TurnState, completed.Err, testAutonomousTurn)
	}
	ready := nextEventOfKind(t, conversation.Events(), ports.ChatEventControllerState)
	if ready.ControllerState == ports.ChatControllerBusy {
		t.Fatal("controller still busy after the turn ended")
	}
	// The conversation takes AO's next prompt again.
	if _, err := conversation.SendTurn(context.Background(), ports.ChatUserMessage{Text: "next"}); err != nil {
		t.Fatalf("SendTurn after the autonomous turn: %v", err)
	}
}

// Stop on such a turn asks the agent to stop that task: there is no prompt for
// session/cancel to stop. The agent reports the stopped task, which ends the turn
// as interrupted.
func TestACPInterruptStopsAutonomousTurn(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	defer conversation.Close()
	// As claude-agent-acp does: the stopped state goes out before the answer.
	agent.setOnStop(func(taskID string) bool {
		agent.send(asyncTaskState(taskID, "stopped", ""))
		return true
	})

	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	_ = nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnStarted)

	if err := conversation.Interrupt(context.Background(), testAutonomousTurn); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	completed := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnCompleted)
	if completed.ProviderTurnID != testAutonomousTurn || completed.TurnState != domain.TurnStateInterrupted || completed.Err != nil {
		t.Fatalf("turn ended %q %q (%v), want interrupted", completed.ProviderTurnID, completed.TurnState, completed.Err)
	}
	if got := agent.stopped(); len(got) != 1 || got[0] != testAutonomousTurn {
		t.Fatalf("async task stops = %v, want [%s]", got, testAutonomousTurn)
	}
	if agent.cancels() != 0 {
		t.Fatalf("session/cancel sent %d times, want none", agent.cancels())
	}
}

// When the agent no longer runs the turn (its end was lost, or the agent was
// replaced), nothing would report its end: Stop ends it.
func TestACPInterruptEndsAutonomousTurnTheAgentNoLongerRuns(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	defer conversation.Close()
	agent.setOnStop(func(string) bool { return false })

	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	_ = nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnStarted)

	if err := conversation.Interrupt(context.Background(), testAutonomousTurn); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	completed := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnCompleted)
	if completed.ProviderTurnID != testAutonomousTurn || completed.TurnState != domain.TurnStateInterrupted {
		t.Fatalf("turn ended %q %q, want interrupted", completed.ProviderTurnID, completed.TurnState)
	}
	if err := conversation.Interrupt(context.Background(), testAutonomousTurn); !errors.Is(err, ports.ErrChatNoActiveTurn) {
		t.Fatalf("second Interrupt = %v, want ErrChatNoActiveTurn", err)
	}
}

// A failed autonomous turn fails with the agent's summary.
func TestACPAutonomousTurnFailureKeepsTheAgentsSummary(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	defer conversation.Close()

	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	agent.send(asyncTaskState(testAutonomousTurn, "failed", "API Error: 529 overloaded"))
	completed := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnCompleted)
	if completed.TurnState != domain.TurnStateFailed || completed.Err == nil ||
		!strings.Contains(completed.Err.Error(), "529 overloaded") {
		t.Fatalf("turn ended %q (%v), want failed with the summary", completed.TurnState, completed.Err)
	}
}

// Other async tasks are not turns, and their updates are not read as anything
// else either: the SDK alone would take a spawn that names a toolCallId for an
// empty tool_call_update.
func TestACPOtherAsyncTasksAreNotTurns(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	defer conversation.Close()

	agent.send(asyncTaskSpawn("task-bash-1", "bash", "tool-9"))
	agent.send(asyncTaskState("task-bash-1", "completed", ""))
	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))

	for {
		event := nextEvent(t, conversation.Events())
		if event.Kind == ports.ChatEventTurnStarted {
			if event.ProviderTurnID != testAutonomousTurn {
				t.Fatalf("started turn = %q, want %q", event.ProviderTurnID, testAutonomousTurn)
			}
			return
		}
		if event.ProviderItemID != "" || strings.HasPrefix(string(event.Kind), "activity.") {
			t.Fatalf("a background task's update became %q for %q", event.Kind, event.ProviderItemID)
		}
	}
}

// Stopping the agent ends its open autonomous turn, which no update would end.
func TestACPTerminateEndsOpenAutonomousTurn(t *testing.T) {
	agent, conversation := startScriptedAgent(t)
	agent.send(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	_ = nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnStarted)

	stopped := make(chan error, 1)
	go func() { stopped <- conversation.(ports.ChatProviderTerminator).Terminate() }()
	completed := nextEventOfKind(t, conversation.Events(), ports.ChatEventTurnCompleted)
	if completed.ProviderTurnID != testAutonomousTurn || completed.TurnState != domain.TurnStateInterrupted {
		t.Fatalf("turn ended %q %q, want interrupted", completed.ProviderTurnID, completed.TurnState)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Terminate: %v", err)
	}
}

// A daemon that attaches again to a host keeps a durable autonomous turn the host
// still runs, settles one that ended while it was away, and opens one the host
// started meanwhile. A durable prompt turn on an idle host is still not
// something it can decide.
func TestACPLiveReconnectRestoresAutonomousTurns(t *testing.T) {
	reconnect := func(t *testing.T, state persistenthost.ACPState, durable string) (*conversation, error) {
		t.Helper()
		_, opened := startScriptedAgent(t)
		t.Cleanup(func() { _ = opened.Close() })
		c := opened.(*conversation)
		c.proc.reconnected = true
		c.proc.gate = newGatedReader(strings.NewReader(""), false)
		c.liveState = &state
		return c, c.ActivateLiveReconnect(context.Background(), durable)
	}

	t.Run("still running", func(t *testing.T) {
		c, err := reconnect(t, persistenthost.ACPState{AutonomousTurn: testAutonomousTurn}, testAutonomousTurn)
		if err != nil {
			t.Fatalf("ActivateLiveReconnect: %v", err)
		}
		c.mu.Lock()
		active, autonomous := c.activeTurn, c.autonomousTurn
		c.mu.Unlock()
		if active != testAutonomousTurn || autonomous != testAutonomousTurn {
			t.Fatalf("active %q, autonomous %q; want both %q", active, autonomous, testAutonomousTurn)
		}
	})
	t.Run("ended while away", func(t *testing.T) {
		c, err := reconnect(t, persistenthost.ACPState{}, testAutonomousTurn)
		if err != nil {
			t.Fatalf("ActivateLiveReconnect: %v", err)
		}
		completed := nextEventOfKind(t, c.Events(), ports.ChatEventTurnCompleted)
		if completed.ProviderTurnID != testAutonomousTurn || completed.TurnState != domain.TurnStateRecovered {
			t.Fatalf("turn ended %q %q, want recovered", completed.ProviderTurnID, completed.TurnState)
		}
	})
	t.Run("started while away", func(t *testing.T) {
		c, err := reconnect(t, persistenthost.ACPState{AutonomousTurn: testAutonomousTurn}, "")
		if err != nil {
			t.Fatalf("ActivateLiveReconnect: %v", err)
		}
		started := nextEventOfKind(t, c.Events(), ports.ChatEventTurnStarted)
		if started.ProviderTurnID != testAutonomousTurn {
			t.Fatalf("started turn = %q, want %q", started.ProviderTurnID, testAutonomousTurn)
		}
	})
	t.Run("prompt turn on an idle host", func(t *testing.T) {
		if _, err := reconnect(t, persistenthost.ACPState{}, "durable-turn"); !errors.Is(err, ports.ErrChatRecoveryInconclusive) {
			t.Fatalf("ActivateLiveReconnect = %v, want ErrChatRecoveryInconclusive", err)
		}
	})
}

// The reader renames only a session/update that carries an async task, and
// counts it; a message that merely mentions one is left alone.
func TestConversationReaderRenamesAsyncTaskUpdates(t *testing.T) {
	spawn, _ := json.Marshal(asyncTaskSpawn(testAutonomousTurn, "turn", ""))
	chatter, _ := json.Marshal(sessionUpdate(map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "text", "text": `I saw "async_task_spawned" in the log`},
	}))
	input := append(append(append(spawn, '\n'), chatter...), '\n')
	barrier := newAsyncTaskBarrier()
	output, err := io.ReadAll(newConversationReader(bytes.NewReader(input), nil, barrier))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("got %d frames, want 2: %s", len(lines), output)
	}
	if !bytes.Contains(lines[0], []byte(`"method":"`+asyncTaskUpdateMethod+`"`)) {
		t.Fatalf("spawn frame = %s, want it renamed", lines[0])
	}
	if !bytes.Equal(lines[1], chatter) {
		t.Fatalf("message frame = %s, want it unchanged", lines[1])
	}
	if barrier.seen != 1 {
		t.Fatalf("async frames seen = %d, want 1", barrier.seen)
	}
}

// A request waits for the async task updates read before it, and no longer.
func TestAsyncTaskBarrierWaitsForUpdatesReadBeforeIt(t *testing.T) {
	barrier := newAsyncTaskBarrier()
	barrier.noteSeen()
	released := make(chan struct{})
	go func() {
		barrier.wait(context.Background(), time.Minute)
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("wait returned before the update was applied")
	case <-time.After(50 * time.Millisecond):
	}
	barrier.noteApplied()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return once the update was applied")
	}

	barrier.noteSeen() // an update that never gets applied
	begun := time.Now()
	barrier.wait(context.Background(), 30*time.Millisecond)
	if waited := time.Since(begun); waited > time.Second {
		t.Fatalf("wait took %v past its limit", waited)
	}
}

const scriptedSessionID = "scripted-session"

// scriptedAgent is an ACP agent on raw JSON frames, for updates the SDK's typed
// agent cannot send. The test writes frames with send; the agent answers
// initialize, session/new, session/close and _session/async_task/stop itself.
type scriptedAgent struct {
	write func(any)

	mu         sync.Mutex
	onStop     func(taskID string) bool
	stops      []string
	cancelSeen int
	responses  map[string]chan json.RawMessage
}

func startScriptedAgent(t *testing.T) (*scriptedAgent, ports.ChatConversation) {
	t.Helper()
	agent := &scriptedAgent{responses: make(map[string]chan json.RawMessage)}
	driver := New(Config{
		Harness: domain.HarnessClaudeCode,
		Capabilities: ports.ChatCapabilities{
			ports.ChatCapabilityStreaming: true,
			ports.ChatCapabilityInterrupt: true,
		},
		Probe: func(context.Context) error { return nil },
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: "fake"}, nil
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	driver.useTestProcess(agent.spawn)
	conversation, err := driver.Start(context.Background(), ports.ChatStartConfig{WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = nextEvent(t, conversation.Events()) // controller.ready
	return agent, conversation
}

func (a *scriptedAgent) spawn(Launch, string) (*process, error) {
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()
	var writeMu sync.Mutex
	encoder := json.NewEncoder(agentToClientW)
	a.write = func(frame any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = encoder.Encode(frame)
	}
	go func() {
		decoder := json.NewDecoder(clientToAgentR)
		for {
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if err := decoder.Decode(&message); err != nil {
				return
			}
			reply := func(result any) {
				a.write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
			}
			switch message.Method {
			case "":
				a.response(string(message.ID)) <- message.Result
			case "initialize":
				reply(map[string]any{
					"protocolVersion": acpsdk.ProtocolVersionNumber, "agentCapabilities": map[string]any{},
					"authMethods": []any{},
				})
			case "session/new":
				reply(map[string]any{"sessionId": scriptedSessionID})
			case "session/close":
				reply(map[string]any{})
			case "session/cancel":
				a.mu.Lock()
				a.cancelSeen++
				a.mu.Unlock()
			case asyncTaskStopMethod:
				var params struct {
					AsyncTaskID string `json:"asyncTaskId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				a.mu.Lock()
				a.stops = append(a.stops, params.AsyncTaskID)
				onStop := a.onStop
				a.mu.Unlock()
				stopped := false
				if onStop != nil {
					stopped = onStop(params.AsyncTaskID)
				}
				reply(map[string]any{"stopped": stopped})
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

func (a *scriptedAgent) send(frame any) { a.write(frame) }

func (a *scriptedAgent) setOnStop(onStop func(taskID string) bool) {
	a.mu.Lock()
	a.onStop = onStop
	a.mu.Unlock()
}

func (a *scriptedAgent) response(id string) chan json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch, ok := a.responses[id]
	if !ok {
		ch = make(chan json.RawMessage, 1)
		a.responses[id] = ch
	}
	return ch
}

func (a *scriptedAgent) nextResponse(t *testing.T, id string) json.RawMessage {
	t.Helper()
	select {
	case result := <-a.response(id):
		return result
	case <-time.After(2 * time.Second):
		t.Fatalf("no answer to agent request %s", id)
		return nil
	}
}

func (a *scriptedAgent) stopped() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.stops...)
}

func (a *scriptedAgent) cancels() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelSeen
}

func sessionUpdate(update map[string]any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": scriptedSessionID, "update": update},
	}
}

// asyncTaskSpawn is the spawn claude-agent-acp sends (async-tasks.js announce).
func asyncTaskSpawn(id, taskType, toolCallID string) map[string]any {
	update := map[string]any{
		"sessionUpdate": asyncTaskSpawned, "asyncTaskId": id, "name": "Claude is working",
		"taskType": taskType, "description": "A message from another session.",
		"showInTranscript": false, "canStop": true,
	}
	if toolCallID != "" {
		update["toolCallId"] = toolCallID
	}
	return sessionUpdate(update)
}

func asyncTaskState(id, state, summary string) map[string]any {
	update := map[string]any{"sessionUpdate": asyncTaskStateUpdate, "asyncTaskId": id, "state": state}
	if summary != "" {
		update["summary"] = summary
	}
	return sessionUpdate(update)
}

// nextEventOfKind skips events of other kinds.
func nextEventOfKind(t *testing.T, events <-chan ports.ChatEvent, kind ports.ChatEventKind) ports.ChatEvent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("event stream closed waiting for %q", kind)
			}
			if event.Kind == kind {
				return event
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", kind)
			return ports.ChatEvent{}
		}
	}
}
