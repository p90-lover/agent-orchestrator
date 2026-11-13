package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type turnsReaderFunc func(context.Context, string) ([]domain.ConversationTurn, error)

func (f turnsReaderFunc) LoadConversationTurns(ctx context.Context, conversationID string) ([]domain.ConversationTurn, error) {
	return f(ctx, conversationID)
}

// activatingLiveConversation records the provider turn the service hands back
// to the live provider once it has restored durable turn ownership.
type activatingLiveConversation struct {
	*liveReconnectedConversation
	mu        sync.Mutex
	activated []string
}

func (c *activatingLiveConversation) ActivateLiveReconnect(_ context.Context, providerTurnID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activated = append(c.activated, providerTurnID)
	return nil
}

func (c *activatingLiveConversation) activatedTurns() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.activated)
}

func newActivatingLiveConversation() *activatingLiveConversation {
	return &activatingLiveConversation{liveReconnectedConversation: &liveReconnectedConversation{
		nativeHistoryConversation: &nativeHistoryConversation{fakeConversation: newFakeConversation()},
	}}
}

// seedDetachedRunningTurn leaves a durably running turn behind a controller that
// was detached the way a daemon restart detaches it, so the provider is still
// working on that turn when the next daemon reconnects.
func seedDetachedRunningTurn(t *testing.T, st *sqlite.Store, newID chatsvc.IDFactory) (providerConversationID, providerTurnID string) {
	t.Helper()
	ctx := context.Background()
	provider := &terminatingConversation{fakeConversation: newFakeConversation()}
	first := chatsvc.New(chatsvc.Options{
		Store: st, Reader: fullSnapshotReader(st), Sessions: st,
		Drivers: fakeRegistry{driver: fakeDriver{conv: provider}},
		Log:     slog.New(slog.DiscardHandler), NewID: newID,
	})
	controller, err := first.Start(ctx, chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	turn, err := controller.Send(ctx, ports.ChatUserMessage{Text: "keep working"})
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	provider.emit(ports.ChatEvent{
		Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID,
		ProviderConversationID: provider.ProviderConversationID(),
	})
	h := &harness{st: st, ctrl: controller}
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, candidate := range s.Turns {
			if candidate.ID == turn.ID {
				return candidate.State == domain.TurnStateRunning
			}
		}
		return false
	})
	first.StopAll(ctx)
	if turn.ProviderTurnID == "" {
		t.Fatal("seeded turn has no provider turn id")
	}
	return provider.ProviderConversationID(), turn.ProviderTurnID
}

// A live reconnect after a daemon restart restores ownership of the turn the
// provider kept running. It must do that from the turns alone: the full snapshot
// also reads every message and activity payload, which on a busy disk outlasted
// the per-chat recovery budget. The fallback (no turns reader wired) must restore
// the very same turn.
func TestServiceLiveReconnectRestoresOwnershipFromTurnsOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		turnsOnly bool
	}{
		{name: "turns reader", turnsOnly: true},
		{name: "snapshot fallback", turnsOnly: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			var ids atomic.Int32
			newID := func() string { return fmt.Sprintf("turns-only-%d", ids.Add(1)) }
			providerConversationID, providerTurnID := seedDetachedRunningTurn(t, st, newID)

			var snapshotReads, turnsReads atomic.Int32
			provider := newActivatingLiveConversation()
			opts := chatsvc.Options{
				Store: st, Sessions: st,
				Reader: chatsvc.SnapshotReaderFunc(func(ctx context.Context, conversationID string) (chatsvc.ConversationRows, error) {
					snapshotReads.Add(1)
					return fullSnapshotReader(st).LoadConversationSnapshot(ctx, conversationID)
				}),
				Drivers: fakeRegistry{driver: fakeDriver{conv: provider}},
				Log:     slog.New(slog.DiscardHandler), NewID: newID,
			}
			if tc.turnsOnly {
				opts.TurnsReader = turnsReaderFunc(func(ctx context.Context, conversationID string) ([]domain.ConversationTurn, error) {
					turnsReads.Add(1)
					return st.LoadConversationTurns(ctx, conversationID)
				})
			}
			svc := chatsvc.New(opts)
			t.Cleanup(func() { svc.StopAll(context.Background()) })
			controller, err := svc.Start(ctx, chatsvc.StartConfig{
				SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
				WorkspacePath: t.TempDir(), ProviderConversationID: providerConversationID,
			})
			if err != nil {
				t.Fatalf("reconnect Start: %v", err)
			}

			if got := provider.activatedTurns(); !slices.Equal(got, []string{providerTurnID}) {
				t.Fatalf("live reconnect activated provider turns %v, want [%s]", got, providerTurnID)
			}
			if got := controller.State(); got != ports.ChatControllerBusy {
				t.Fatalf("controller state after reconnect = %q, want busy for the durable running turn", got)
			}
			if tc.turnsOnly {
				if got := snapshotReads.Load(); got != 0 {
					t.Fatalf("live reconnect read the full snapshot (messages and activities) %d times, want 0", got)
				}
				if got := turnsReads.Load(); got != 1 {
					t.Fatalf("live reconnect turns reads = %d, want 1", got)
				}
			} else if got := snapshotReads.Load(); got != 1 {
				t.Fatalf("fallback live reconnect snapshot reads = %d, want 1", got)
			}
		})
	}
}

// A turns read that fails leaves the reconnect inconclusive, exactly as a failed
// full snapshot did: guessing "no running turn" could start a second root turn on
// a provider that is still busy.
func TestServiceLiveReconnectTurnsReadFailureIsInconclusive(t *testing.T) {
	st := openStore(t)
	var ids atomic.Int32
	newID := func() string { return fmt.Sprintf("turns-fail-%d", ids.Add(1)) }
	providerConversationID, _ := seedDetachedRunningTurn(t, st, newID)

	readErr := errors.New("turns read timed out")
	provider := newActivatingLiveConversation()
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		TurnsReader: turnsReaderFunc(func(context.Context, string) ([]domain.ConversationTurn, error) {
			return nil, readErr
		}),
		Drivers: fakeRegistry{driver: fakeDriver{conv: provider}},
		Log:     slog.New(slog.DiscardHandler), NewID: newID,
	})
	t.Cleanup(func() { svc.StopAll(context.Background()) })
	_, err := svc.Start(context.Background(), chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: providerConversationID,
	})
	if !errors.Is(err, ports.ErrChatRecoveryInconclusive) || !errors.Is(err, readErr) {
		t.Fatalf("reconnect Start error = %v, want inconclusive wrapping the read error", err)
	}
	if got := provider.activatedTurns(); len(got) != 0 {
		t.Fatalf("a failed turns read still activated provider turns %v", got)
	}
}
