package chat_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// startRunningTurn sends text and waits until its turn shows as running.
func startRunningTurn(t *testing.T, h *harness, text, clientID, providerTurnID string) {
	t.Helper()
	if _, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: text, ClientMessageID: clientID, Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: providerTurnID})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)[text] == domain.TurnStateRunning
	})
}

func queueBehindRunningTurn(t *testing.T, h *harness, text, clientID string) {
	t.Helper()
	turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: text, ClientMessageID: clientID, Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("queue %q: %v", text, err)
	}
	if turn.State != domain.TurnStateQueued {
		t.Fatalf("%q was %q on send, want queued behind the running turn", text, turn.State)
	}
}

// Stop pressed twice while the agent winds down. The provider refuses the second
// one (ACP: "session/cancel is already in progress"), and that refusal used to
// erase the first Stop's cutoff: the interrupted turn then neither cancelled what
// was queued before Stop nor sent what was typed after it. Both sat queued with
// nothing running.
func TestSecondStopThatFailsKeepsTheFirstStopsQueueCutoff(t *testing.T) {
	conv := newInterruptRecorder()
	h := newHarnessWithConversation(t, conv)
	ctx := context.Background()

	startRunningTurn(t, h, "running", "c1", "provider-turn-1")
	conv.markActive("provider-turn-1")
	queueBehindRunningTurn(t, h, "before stop", "c2")

	if err := h.svc.Interrupt(ctx, testSession); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	h.advance(time.Second)
	conv.activeMu.Lock()
	conv.interruptErr = errors.New("ACP session/cancel is already in progress")
	conv.activeMu.Unlock()
	if err := h.svc.Interrupt(ctx, testSession); err == nil {
		t.Fatal("second Stop: the provider's refusal must reach the caller")
	}

	h.advance(time.Second)
	queueBehindRunningTurn(t, h, "after stop", "c3")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateInterrupted,
	})

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["after stop"] == domain.TurnStateRunning
	})
	if got := turnStateByText(t, snapshot)["before stop"]; got != domain.TurnStateInterrupted {
		t.Errorf("message queued before Stop = %q, want cancelled with the turn", got)
	}
	if got := h.conv.sentTexts(); !slices.Equal(got, []string{"running", "after stop"}) {
		t.Fatalf("provider received %v, want the running turn then the post-Stop message", got)
	}
}

// A drain that finds the stopped turn still running has nothing to do, and it
// must leave the Stop's cutoff for that turn's completion. It used to read and
// clear the cutoff before looking, so a message arriving mid-wind-down (which
// drains) lost the Stop the same way a failed second Stop did.
func TestDrainWhileTheStoppedTurnStillRunsKeepsTheCutoff(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	startRunningTurn(t, h, "running", "c1", "provider-turn-1")
	queueBehindRunningTurn(t, h, "before stop", "c2")
	if err := h.svc.Interrupt(ctx, testSession); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatalf("drain while the turn winds down: %v", err)
	}

	h.advance(time.Second)
	queueBehindRunningTurn(t, h, "after stop", "c3")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateInterrupted,
	})

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["after stop"] == domain.TurnStateRunning
	})
	if got := turnStateByText(t, snapshot)["before stop"]; got != domain.TurnStateInterrupted {
		t.Errorf("message queued before Stop = %q, want cancelled with the turn", got)
	}
}

// sqliteBusy is the store's "the writer was taken" answer, as the driver reports it.
type sqliteBusy struct{}

func (sqliteBusy) Error() string { return "database is locked (5) (SQLITE_BUSY)" }
func (sqliteBusy) Code() int     { return 5 }

// busyOnceStore refuses the first projection of one event kind as busy, writing
// nothing, and takes every later one.
type busyOnceStore struct {
	chatsvc.Store
	method  string
	refused atomic.Int32
}

func (s *busyOnceStore) ProjectProviderEvent(
	ctx context.Context, conversationID string, session domain.SessionID,
	generation, providerEventID, method, payloadJSON string, now time.Time,
	project func(context.Context) error,
) (bool, error) {
	if method == s.method && s.refused.CompareAndSwap(0, 1) {
		return false, sqliteBusy{}
	}
	return s.Store.ProjectProviderEvent(ctx, conversationID, session, generation,
		providerEventID, method, payloadJSON, now, project)
}

// A busy store is a wait, not a fault, and a persistent (ACP) chat waits it out.
// A Codex chat got one try: its turn-completed event was dropped, the turn stayed
// "running", the controller stayed busy and every later message queued for good.
func TestBusyStoreOnTurnCompletedIsRetriedWithoutAPersistentDriver(t *testing.T) {
	busy := &busyOnceStore{method: string(ports.ChatEventTurnCompleted)}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		busy.Store = st
		return busy
	})

	startRunningTurn(t, h, "running", "c1", "provider-turn-1")
	queueBehindRunningTurn(t, h, "queued", "c2")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateCompleted,
	})

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["queued"] == domain.TurnStateRunning
	})
	if busy.refused.Load() != 1 {
		t.Fatal("the store never refused the completion as busy; the test proved nothing")
	}
	if got := turnStateByText(t, snapshot)["running"]; got != domain.TurnStateCompleted {
		t.Errorf("turn whose completion met a busy store = %q, want completed", got)
	}
	if got := h.conv.sentTexts(); !slices.Equal(got, []string{"running", "queued"}) {
		t.Fatalf("provider received %v, want the queued message sent once the turn settled", got)
	}
}

// A failed turn holds the queue (#4861). The next message the user sends used to
// jump that queue and run first, and the older message ran after it. It now goes
// behind what is already waiting, and sending it is what releases the hold.
func TestSendAfterAFailedTurnGoesBehindTheOlderQueuedMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	startRunningTurn(t, h, "root", "c1", "provider-turn-1")
	queueBehindRunningTurn(t, h, "older follow-up", "c2")
	h.conv.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1",
		TurnState: domain.TurnStateFailed,
	})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["root"] == domain.TurnStateFailed
	})
	assertTurnStaysQueued(t, h, "older follow-up")

	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
		Text: "newer", ClientMessageID: "c3", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("send after the failed turn: %v", err)
	}
	if turn.State != domain.TurnStateQueued {
		t.Errorf("newer message was %q on send, want queued behind the older one", turn.State)
	}
	if got := h.conv.sentTexts(); !slices.Equal(got, []string{"root", "older follow-up"}) {
		t.Fatalf("provider received %v, want the older queued message before the newer one", got)
	}

	h.conv.emit(
		ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-2"},
		ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-2", TurnState: domain.TurnStateCompleted},
	)
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return turnStateByText(t, s)["newer"] == domain.TurnStateRunning
	})
	if got := h.conv.sentTexts(); !slices.Equal(got, []string{"root", "older follow-up", "newer"}) {
		t.Fatalf("provider received %v, want all three in the order they were typed", got)
	}
}

// With nothing older waiting, a message sent to an idle chat still goes straight out.
func TestSendToAnIdleChatWithNothingQueuedDispatchesAtOnce(t *testing.T) {
	h := newHarness(t)

	turn, err := h.svc.Send(context.Background(), testSession, ports.ChatUserMessage{
		Text: "only", ClientMessageID: "c1", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if turn.State == domain.TurnStateQueued {
		t.Errorf("message sent to an idle chat came back queued")
	}
	if got := h.conv.sentTexts(); !slices.Equal(got, []string{"only"}) {
		t.Fatalf("provider received %v, want the message at once", got)
	}
}
