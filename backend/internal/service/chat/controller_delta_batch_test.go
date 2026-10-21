package chat_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// A burst of streaming deltas is committed in batches, yet every delta is applied
// exactly once and acknowledged in order before the turn's completion, including
// when a batch rolls back and its events are projected again one at a time.
func TestStreamingDeltaBatchesApplyOnceAndAcknowledgeInOrder(t *testing.T) {
	for _, failures := range []int32{0, 1} {
		name := "batched"
		if failures > 0 {
			name = "rolled-back batch is projected per event"
		}
		t.Run(name, func(t *testing.T) {
			const deltas = 40
			acknowledged := make(chan string, deltas+2)
			conv := &receiptConversation{
				terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
				ack:                     func(id string) error { acknowledged <- id; return nil },
			}
			counting := &countingBatchStore{}
			h := newHarnessWithConversationAndStore(t, conv, func(st *sqlite.Store) chatsvc.Store {
				flaky := &flakyReceiptStore{Store: st}
				flaky.failures.Store(failures)
				counting.Store = flaky
				return counting
			})
			turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "stream"})
			if err != nil {
				t.Fatal(err)
			}
			events := []ports.ChatEvent{{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID}}
			var want strings.Builder
			for i := range deltas {
				delta := fmt.Sprintf("d%02d ", i)
				want.WriteString(delta)
				events = append(events, ports.ChatEvent{
					Kind: ports.ChatEventMessageDelta, ProviderTurnID: turn.ProviderTurnID,
					ProviderItemID: "answer", ProviderEventID: fmt.Sprintf("delta-%02d", i), Delta: delta,
				})
			}
			events = append(events, ports.ChatEvent{
				Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID,
				ProviderEventID: "completion", TurnState: domain.TurnStateCompleted,
			})
			conv.emit(events...)

			for i := range deltas + 1 {
				wantID := "completion"
				if i < deltas {
					wantID = fmt.Sprintf("delta-%02d", i)
				}
				select {
				case got := <-acknowledged:
					if got != wantID {
						t.Fatalf("ack %d = %q, want %q", i, got, wantID)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("missing ack %s", wantID)
				}
			}
			snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
			})
			if len(snapshot.Messages) != 2 || snapshot.Messages[1].Text != want.String() {
				t.Fatalf("streamed text not applied exactly once: %+v", snapshot.Messages)
			}
			if batches := counting.batches.Load(); batches == 0 || batches >= deltas {
				t.Fatalf("%d deltas used %d batch transactions, want several deltas per batch", deltas, batches)
			}
		})
	}
}

// A batch writes each streamed row once: interleaved reasoning and message deltas
// land as the exact text they stream, but the growing rows are appended to once
// per batch, not once per delta.
func TestStreamingDeltaBatchAppendsEachRowOncePerBatch(t *testing.T) {
	appends := &countingAppendStore{}
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		appends.Store = st
		return appends
	})
	turn, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "think"})
	if err != nil {
		t.Fatal(err)
	}
	events := []ports.ChatEvent{
		{Kind: ports.ChatEventTurnStarted, ProviderTurnID: turn.ProviderTurnID},
		{Kind: ports.ChatEventActivityStarted, ProviderTurnID: turn.ProviderTurnID, ProviderItemID: "rs_1",
			ActivityKind: domain.ActivityKindReasoning, ActivityStatus: domain.ActivityStatusRunning, Summary: "Reasoning"},
	}
	const deltas = 60
	var reasoning, answer strings.Builder
	for i := range deltas {
		delta := fmt.Sprintf("r%02d ", i)
		reasoning.WriteString(delta)
		events = append(events, ports.ChatEvent{Kind: ports.ChatEventReasoningDelta, ProviderTurnID: turn.ProviderTurnID,
			ProviderItemID: "rs_1", ProviderEventID: fmt.Sprintf("reasoning-%02d", i), Delta: delta})
		if i%3 == 0 {
			delta = fmt.Sprintf("a%02d ", i)
			answer.WriteString(delta)
			events = append(events, ports.ChatEvent{Kind: ports.ChatEventMessageDelta, ProviderTurnID: turn.ProviderTurnID,
				ProviderItemID: "answer", ProviderEventID: fmt.Sprintf("answer-%02d", i), Delta: delta})
		}
	}
	h.conv.emit(events...)

	snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		return activityByItem(s, "rs_1").StreamedText == reasoning.String() &&
			len(s.Messages) == 2 && s.Messages[1].Text == answer.String()
	})
	if got := activityByItem(snapshot, "rs_1").StreamedText; got != reasoning.String() {
		t.Fatalf("reasoning = %q", got)
	}
	if n := appends.activity.Load(); n == 0 || n >= deltas/2 {
		t.Fatalf("%d reasoning deltas took %d activity appends, want a few per batch", deltas, n)
	}
	if n := appends.message.Load(); n == 0 || n >= deltas/3 {
		t.Fatalf("%d answer deltas took %d message appends, want a few per batch", deltas/3, n)
	}
}

type countingAppendStore struct {
	chatsvc.Store
	activity, message atomic.Int32
}

func (s *countingAppendStore) AppendActivityStreamedText(ctx context.Context, conversationID, providerItemID, delta string, now time.Time) (bool, error) {
	s.activity.Add(1)
	return s.Store.AppendActivityStreamedText(ctx, conversationID, providerItemID, delta, now)
}

func (s *countingAppendStore) AppendAssistantDelta(ctx context.Context, conversationID, providerItemID, providerTurnID, delta, messageID string, now time.Time) error {
	s.message.Add(1)
	return s.Store.AppendAssistantDelta(ctx, conversationID, providerItemID, providerTurnID, delta, messageID, now)
}

type countingBatchStore struct {
	chatsvc.Store
	batches atomic.Int32
}

func (s *countingBatchStore) InProviderEventBatch(ctx context.Context, fn func(context.Context) error) error {
	s.batches.Add(1)
	return s.Store.InProviderEventBatch(ctx, fn)
}
