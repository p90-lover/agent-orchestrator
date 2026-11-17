package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Claude Code can start a model turn with no prompt open: a scheduled wake-up
// fired, a background task finished, or another local Claude session sent it a
// message. claude-agent-acp reports such a turn only to a client that declares
// the AIR `asyncTasks` capability, as an async task of type "turn" named
// "Claude is working". Without it, AO stored the turn's output with no turn, and
// the turn's first permission request was never shown: the chat read Done while
// the agent waited for hours (2026-11-09, coding-tools-mcp-278).
//
// AO declares the capability and adopts that task as a turn the provider
// started. The controller already adopts such turns (AdoptProviderTurn), so the
// turn gets a row, its output and approvals carry its id, and Stop interrupts it.
const (
	airAsyncTasksCapability = "asyncTasks"

	asyncTaskSpawned     = "async_task_spawned"
	asyncTaskStateUpdate = "async_task_state_update"
	// autonomousTurnTaskType is the task type claude-agent-acp gives a turn that
	// Claude Code started by itself.
	autonomousTurnTaskType = "turn"
	// autonomousTurnIDPrefix is how claude-agent-acp names that task. AO reads it
	// only to recognise such a turn after a restart (ActivateLiveReconnect).
	autonomousTurnIDPrefix = "autonomous-turn-"
	// asyncTaskStopMethod stops one async task. For an autonomous turn the agent
	// interrupts that turn.
	asyncTaskStopMethod = "_session/async_task/stop"
	// asyncTaskUpdateMethod is AO's own name for a session/update that carries an
	// async task. extensionMethodReader renames those frames before the SDK sees
	// them: the SDK's SessionUpdate union has no async task variants, and its
	// structural fallback reads one as an empty session_info_update, or, when it
	// names a toolCallId, as an empty tool_call_update. The name never reaches
	// the wire.
	asyncTaskUpdateMethod = "_ao/async_task_update"

	// asyncTaskOrderWait bounds how long an agent request waits for the async task
	// updates that came before it (asyncTaskBarrier).
	asyncTaskOrderWait = 2 * time.Second
	// autonomousTurnSettleWait bounds how long an autonomous turn's start waits for
	// a prompt that has answered but is still being settled.
	autonomousTurnSettleWait = 2 * time.Second
)

var asyncTaskFrameMarker = []byte(`"async_task_`)

type asyncTaskUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		AsyncTaskID   string `json:"asyncTaskId"`
		TaskType      string `json:"taskType"`
		State         string `json:"state"`
		Summary       string `json:"summary"`
	} `json:"update"`
}

// isAsyncTaskUpdate reports whether a session/update's params carry an async
// task update.
func isAsyncTaskUpdate(params json.RawMessage) bool {
	var probe struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &probe) != nil {
		return false
	}
	return strings.HasPrefix(probe.Update.SessionUpdate, "async_task_")
}

// asyncTaskBarrier lets a request from the agent wait until the async task
// updates that came before it on the wire are applied. The SDK hands
// notifications to one goroutine in order, but runs each request on a goroutine
// of its own. Without the wait, the permission request of a turn the agent just
// started could be handled before the update that opens the turn, and be filed
// under no turn: hidden from the chat, which is the failure this file fixes.
type asyncTaskBarrier struct {
	mu      sync.Mutex
	seen    uint64
	applied uint64
	moved   chan struct{}
}

func newAsyncTaskBarrier() *asyncTaskBarrier {
	return &asyncTaskBarrier{moved: make(chan struct{})}
}

// noteSeen counts an async task frame as the reader passes it to the SDK.
func (b *asyncTaskBarrier) noteSeen() {
	b.mu.Lock()
	b.seen++
	b.mu.Unlock()
}

// noteApplied counts an async task update as handled.
func (b *asyncTaskBarrier) noteApplied() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.applied++
	close(b.moved)
	b.moved = make(chan struct{})
	b.mu.Unlock()
}

// wait returns once every async task frame read so far is applied, or after
// limit. The reader may already be past the request (the SDK reads ahead), so
// this can also wait for a later update; that only delays the request a little.
func (b *asyncTaskBarrier) wait(ctx context.Context, limit time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	target := b.seen
	b.mu.Unlock()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if b.applied >= target {
			b.mu.Unlock()
			return
		}
		moved := b.moved
		b.mu.Unlock()
		select {
		case <-moved:
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		}
	}
}

// applyAsyncTaskUpdate handles one async task update. Only an autonomous turn is
// adopted. The other task types (a backgrounded command, a Monitor, a background
// subagent) are left out: AO turns those tools off for Claude, and they are not
// turns of their own.
func (c *conversation) applyAsyncTaskUpdate(raw json.RawMessage) {
	defer c.asyncTasks.noteApplied()
	var params asyncTaskUpdateParams
	if err := json.Unmarshal(raw, &params); err != nil {
		c.log.Warn("ignored an unreadable ACP async task update", "err", err)
		return
	}
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	if sessionID != "" && params.SessionID != sessionID {
		return
	}
	update := params.Update
	switch update.SessionUpdate {
	case asyncTaskSpawned:
		if update.TaskType == autonomousTurnTaskType {
			c.autonomousTurnStarted(update.AsyncTaskID)
		}
	case asyncTaskStateUpdate:
		if state, turnErr, ended := autonomousTurnEnd(update.State, update.Summary); ended {
			c.finishAutonomousTurn(update.AsyncTaskID, state, turnErr)
		}
	}
}

// autonomousTurnEnd maps an async task's terminal state onto the turn's.
func autonomousTurnEnd(state, summary string) (domain.TurnState, error, bool) {
	switch state {
	case "completed":
		return domain.TurnStateCompleted, nil, true
	case "stopped":
		return domain.TurnStateInterrupted, nil, true
	case "failed":
		message := strings.TrimSpace(summary)
		if message == "" {
			message = "the turn Claude Code started on its own failed"
		}
		return domain.TurnStateFailed, errors.New(message), true
	default:
		return "", nil, false
	}
}

// autonomousTurnStarted opens the turn the agent started by itself, unless AO
// has a turn of its own in flight. AO keeps the chat busy while a turn runs, so
// that happens only when AO's prompt and the agent's turn cross; the agent then
// folds the prompt into its turn, and the output is filed under AO's prompt.
func (c *conversation) autonomousTurnStarted(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	if c.closed || c.autonomousTurn == id {
		c.mu.Unlock()
		return
	}
	previous := c.autonomousTurn
	c.mu.Unlock()
	if previous != "" {
		// The agent runs one autonomous turn at a time, so the end of the last one
		// was lost.
		c.finishAutonomousTurn(previous, domain.TurnStateRecovered, nil)
	}

	c.mu.Lock()
	// A prompt's result can arrive just before the turn the agent starts after it.
	// The prompt releases the conversation as it is settled; let it finish.
	if c.activeTurn != "" && c.turnDone != nil {
		done := c.turnDone
		c.mu.Unlock()
		select {
		case <-done:
		case <-time.After(autonomousTurnSettleWait):
		}
		c.mu.Lock()
	}
	if c.closed || c.activeTurn != "" || c.prepared != nil || c.compactingTurnID != "" {
		busy := c.activeTurn
		c.mu.Unlock()
		c.log.Debug("ACP agent started a turn of its own while AO had one in flight",
			"task", id, "turn", busy)
		return
	}
	c.autonomousTurn = id
	c.activeTurn = id
	c.settlingTurn = ""
	c.turnCancel = nil
	c.turnDone = nil
	c.messages = make(map[string]string)
	c.thoughts = make(map[string]string)
	c.nestedMessages = make(map[string]nestedMessageState)
	c.tools = make(map[string]*toolState)
	c.turnDiffs = nil
	c.turnDiffTurnID = ""
	c.providerFailure = nil
	c.mu.Unlock()

	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id})
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerBusy})
}

// finishAutonomousTurn reports the end of the autonomous turn id. It does nothing
// for a turn that is not open, so a repeated or late end is harmless.
//
// It never waits for an interrupt in flight. It runs on the SDK's notification
// goroutine, and the agent reports a stopped turn before it answers the stop, so
// the stop's answer waits for this update to be handled.
func (c *conversation) finishAutonomousTurn(id string, state domain.TurnState, turnErr error) {
	c.mu.Lock()
	if id == "" || c.autonomousTurn != id {
		c.mu.Unlock()
		return
	}
	c.autonomousTurn = ""
	owned := c.activeTurn == id
	if owned {
		c.settlingTurn = id
	}
	c.mu.Unlock()

	if owned {
		c.settleOpenItems(id, state)
	}
	c.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, TurnState: state, Err: turnErr,
	})
	c.mu.Lock()
	if c.activeTurn == id {
		c.activeTurn = ""
		c.settlingTurn = ""
		c.providerFailure = nil
		if c.interrupt != nil && c.interrupt.turnID == id {
			c.interrupt = nil
		}
	}
	c.mu.Unlock()
	if owned {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: c.idleControllerState()})
	}
}

// abandonAutonomousTurn ends the open autonomous turn as interrupted when its
// agent goes away. Nothing else would report its end.
func (c *conversation) abandonAutonomousTurn() {
	c.mu.Lock()
	id := c.autonomousTurn
	c.mu.Unlock()
	c.finishAutonomousTurn(id, domain.TurnStateInterrupted, nil)
}

// interruptAutonomousTurn stops the turn the agent started by itself. Called by
// Interrupt with c.interrupt set to attempt. The agent reports the stopped turn
// as an update, which ends it.
func (c *conversation) interruptAutonomousTurn(
	ctx context.Context,
	sessionID string,
	attempt *interruptAttempt,
) error {
	raw, err := c.conn.CallExtension(ctx, asyncTaskStopMethod, map[string]any{
		"sessionId": sessionID, "asyncTaskId": attempt.turnID,
	})
	var response struct {
		Stopped bool `json:"stopped"`
	}
	if err == nil {
		if decodeErr := json.Unmarshal(raw, &response); decodeErr != nil {
			err = fmt.Errorf("decode ACP async task stop: %w", decodeErr)
		}
	}
	c.mu.Lock()
	attempt.err = err
	close(attempt.done)
	if err != nil && c.interrupt == attempt {
		c.interrupt = nil
	}
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("ACP %s: %w", asyncTaskStopMethod, err)
	}
	if !response.Stopped {
		// The agent has no such turn running: its end was lost, or the agent was
		// replaced. Nothing will report it, so end it here.
		c.finishAutonomousTurn(attempt.turnID, domain.TurnStateInterrupted, nil)
	}
	return nil
}

// isAutonomousTurnID reports whether a durable provider turn id names a turn
// claude-agent-acp started by itself.
func isAutonomousTurnID(id string) bool {
	return strings.HasPrefix(id, autonomousTurnIDPrefix)
}

// clientAIRCapabilities are the AIR extension capabilities AO declares.
// sessionFailure: the agent publishes retryable API and transport failures.
// asyncTasks: the agent reports the turns Claude Code starts by itself.
func clientAIRCapabilities() []string {
	return []string{"sessionFailure", airAsyncTasksCapability}
}

// rewriteAsyncTaskFrame renames a session/update that carries an async task to
// asyncTaskUpdateMethod. ok is false for every other frame.
func rewriteAsyncTaskFrame(envelope map[string]json.RawMessage, method string) ([]byte, bool) {
	if method != string(acpsdk.ClientMethodSessionUpdate) || !isAsyncTaskUpdate(envelope["params"]) {
		return nil, false
	}
	renamed, _ := json.Marshal(asyncTaskUpdateMethod)
	envelope["method"] = renamed
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, false
	}
	return append(encoded, '\n'), true
}

func hasAsyncTaskMarker(line []byte) bool {
	return bytes.Contains(line, asyncTaskFrameMarker)
}
