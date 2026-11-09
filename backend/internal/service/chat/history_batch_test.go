package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

// A resumed chat's native history goes in a batch of events per transaction, not
// one transaction per event: on a busy disk the per-event writes made a long chat's
// resume outlast AO's request limit. Every event is still imported exactly once,
// also when a batch rolls back and its events go in one at a time.
func TestResumeImportsNativeHistoryInBatches(t *testing.T) {
	const activities = 300
	history := []ports.ChatEvent{
		{Kind: ports.ChatEventTurnStarted, ProviderEventID: "start", ProviderTurnID: "native-turn"},
		{
			Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "user",
			ProviderTurnID: "native-turn", ProviderItemID: "user-item", Text: "Look around",
		},
	}
	for i := range activities {
		history = append(history, ports.ChatEvent{
			Kind: ports.ChatEventActivityCompleted, ProviderEventID: fmt.Sprintf("command-%03d", i),
			ProviderTurnID: "native-turn", ProviderItemID: fmt.Sprintf("command-item-%03d", i),
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted,
			Summary: fmt.Sprintf("Ran step %d", i), Detail: json.RawMessage(fmt.Sprintf(`{"command":"step %d"}`, i)),
		})
	}
	history = append(history,
		ports.ChatEvent{
			Kind: ports.ChatEventMessageCompleted, ProviderEventID: "answer",
			ProviderTurnID: "native-turn", ProviderItemID: "answer-item", Text: "Done.",
		},
		ports.ChatEvent{
			Kind: ports.ChatEventTurnCompleted, ProviderEventID: "complete",
			ProviderTurnID: "native-turn", TurnState: domain.TurnStateCompleted,
		},
	)
	batches := (len(history) + 127) / 128

	for _, failFirst := range []bool{false, true} {
		name := "batched"
		if failFirst {
			name = "rolled-back batch goes in event by event"
		}
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			counting := &historyBatchStore{Store: st}
			if failFirst {
				counting.failures.Store(1)
			}
			conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: history}
			conv.providerConversationID = "native-thread"
			var nextID atomic.Int64
			svc := chatsvc.New(chatsvc.Options{
				Store: counting, Sessions: st,
				Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
				Log:     slog.New(slog.DiscardHandler),
				NewID:   func() string { return fmt.Sprintf("history-batch-id-%d", nextID.Add(1)) },
			})
			t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })

			ctrl, err := svc.Start(context.Background(), chatsvc.StartConfig{
				SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
				WorkspacePath: t.TempDir(), ProviderConversationID: "native-thread",
				HistoryMode: ports.ChatHistoryImport,
			})
			if err != nil {
				t.Fatalf("Start resume: %v", err)
			}
			snapshot, err := st.LoadConversationSnapshot(context.Background(), ctrl.ConversationID())
			if err != nil {
				t.Fatalf("LoadConversationSnapshot: %v", err)
			}
			if len(snapshot.Activities) != activities {
				t.Fatalf("imported %d activities, want %d", len(snapshot.Activities), activities)
			}
			for i, activity := range snapshot.Activities {
				if want := fmt.Sprintf("Ran step %d", i); activity.Summary != want {
					t.Fatalf("activity %d = %q, want %q (out of order or duplicated)", i, activity.Summary, want)
				}
			}
			if len(snapshot.Messages) != 2 || snapshot.Messages[0].Text != "Look around" || snapshot.Messages[1].Text != "Done." {
				t.Fatalf("imported messages = %#v", snapshot.Messages)
			}
			if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateCompleted {
				t.Fatalf("imported turns = %#v", snapshot.Turns)
			}

			wantBatches, wantAlone := int32(batches), int32(0)
			if failFirst {
				// The rolled-back first batch is tried once and then goes in event by event.
				wantAlone = 128
			}
			if got := counting.batches.Load(); got != wantBatches {
				t.Fatalf("history import took %d batch transactions, want %d", got, wantBatches)
			}
			if got := counting.alone.Load(); got != wantAlone {
				t.Fatalf("%d history events took a transaction of their own, want %d", got, wantAlone)
			}
		})
	}
}

// historyBatchStore counts provider-event transactions: batches, and events
// projected outside any batch (each one its own transaction). failures rolls
// back that many batches after their events were applied.
type historyBatchStore struct {
	chatsvc.Store
	batches, alone, depth, failures atomic.Int32
}

var errInjectedBatchRollback = errors.New("injected batch rollback")

func (s *historyBatchStore) InProviderEventBatch(ctx context.Context, fn func(context.Context) error) error {
	s.batches.Add(1)
	s.depth.Add(1)
	defer s.depth.Add(-1)
	return s.Store.InProviderEventBatch(ctx, func(txCtx context.Context) error {
		if err := fn(txCtx); err != nil {
			return err
		}
		if s.failures.Load() > 0 {
			s.failures.Add(-1)
			return errInjectedBatchRollback
		}
		return nil
	})
}

func (s *historyBatchStore) ProjectProviderEvent(ctx context.Context, conversationID string, session domain.SessionID, generation, providerEventID, method, payloadJSON string, now time.Time, project func(context.Context) error) (bool, error) {
	if s.depth.Load() == 0 {
		s.alone.Add(1)
	}
	return s.Store.ProjectProviderEvent(ctx, conversationID, session, generation, providerEventID, method, payloadJSON, now, project)
}
