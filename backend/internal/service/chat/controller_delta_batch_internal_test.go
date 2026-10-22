package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCollectChatDeltaBatchStopsAtOtherEventsLimitWindowAndClose(t *testing.T) {
	delta := func(id string) ports.ChatEvent {
		return ports.ChatEvent{Kind: ports.ChatEventReasoningDelta, ProviderEventID: id}
	}

	events := make(chan ports.ChatEvent, 8)
	events <- delta("b")
	events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "done"}
	events <- delta("c")
	batch, held, closed := collectChatDeltaBatch(events, delta("a"), time.Second, 10)
	if len(batch) != 2 || batch[1].ProviderEventID != "b" || held == nil || held.ProviderEventID != "done" || closed {
		t.Fatalf("other kind must end the batch and be held next: batch=%v held=%v closed=%v", batch, held, closed)
	}

	events = make(chan ports.ChatEvent, 8)
	for _, id := range []string{"b", "c", "d"} {
		events <- delta(id)
	}
	if batch, held, _ = collectChatDeltaBatch(events, delta("a"), time.Second, 3); len(batch) != 3 || held != nil {
		t.Fatalf("limit must cap the batch: batch=%v held=%v", batch, held)
	}

	started := time.Now()
	batch, held, closed = collectChatDeltaBatch(make(chan ports.ChatEvent), delta("a"), 20*time.Millisecond, 10)
	if len(batch) != 1 || held != nil || closed || time.Since(started) > time.Second {
		t.Fatalf("an idle stream must end the batch at the window: batch=%v held=%v closed=%v", batch, held, closed)
	}

	events = make(chan ports.ChatEvent, 1)
	events <- delta("b")
	close(events)
	if batch, _, closed = collectChatDeltaBatch(events, delta("a"), time.Second, 10); len(batch) != 2 || !closed {
		t.Fatalf("stream end must flush the batch and report closed: batch=%v closed=%v", batch, closed)
	}
}

func TestStreamedRowsGatherLongerOnceLarge(t *testing.T) {
	output := func(item, text string) ports.ChatEvent {
		return ports.ChatEvent{Kind: ports.ChatEventCommandOutputDelta, ProviderItemID: item, Delta: text}
	}
	streamed := streamedChars{}
	if got := streamed.window(output("build", "x")); got != chatDeltaBatchWindow {
		t.Fatalf("a new row's window = %v, want %v", got, chatDeltaBatchWindow)
	}
	streamed.add([]ports.ChatEvent{output("build", strings.Repeat("x", largeStreamChars-1))})
	if got := streamed.window(output("build", "x")); got != chatDeltaBatchWindow {
		t.Fatalf("a row under the size keeps the one-second window, got %v", got)
	}
	streamed.add([]ports.ChatEvent{output("build", "x")})
	if got := streamed.window(output("build", "x")); got != chatDeltaBatchWindowLarge {
		t.Fatalf("a large row's window = %v, want %v", got, chatDeltaBatchWindowLarge)
	}
	// Each row is counted on its own, and an event that appends nothing never waits longer.
	if got := streamed.window(output("test", "x")); got != chatDeltaBatchWindow {
		t.Fatalf("another row's window = %v, want %v", got, chatDeltaBatchWindow)
	}
	reasoning := ports.ChatEvent{Kind: ports.ChatEventReasoningDelta, ProviderItemID: "build", Delta: "x"}
	if got := streamed.window(reasoning); got != chatDeltaBatchWindow {
		t.Fatalf("reasoning on the same item is another row; window = %v", got)
	}
	if got := streamed.window(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted}); got != chatDeltaBatchWindow {
		t.Fatalf("a non-delta event's window = %v", got)
	}

	for i := 0; i <= maxStreamedRows; i++ {
		streamed.add([]ports.ChatEvent{output(fmt.Sprint("row-", i), "x")})
	}
	if len(streamed) > maxStreamedRows+1 {
		t.Fatalf("finished rows must not pile up: %d tracked", len(streamed))
	}
}

func TestCompactArchiveRecordDropsAbsentFields(t *testing.T) {
	payload, err := compactArchiveRecord(map[string]any{
		"kind": ports.ChatEventReasoningDelta, "providerEventId": "e1", "delta": "abc",
		"text": "", "usage": nil, "providerItemAliases": []string{}, "detail": json.RawMessage("null"),
		"decisions": map[string]any{}, "turnState": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload != `{"delta":"abc","kind":"reasoning.delta","providerEventId":"e1"}` {
		t.Fatalf("payload = %s", payload)
	}
}
