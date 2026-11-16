package acp

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// spillConversation is a host-journaled conversation (proc.terminate set) with a
// tiny events channel, a real SDK connection over pipes, and a func that ends
// that connection the way a closed agent pipe does.
func spillConversation(t *testing.T, buffer int) (*conversation, func()) {
	t.Helper()
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, clientToAgentR) }()
	c := &conversation{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		proc:      &process{terminate: func() error { return nil }},
		events:    make(chan ports.ChatEvent, buffer),
		spillRoom: make(chan struct{}, 1),
	}
	c.conn = acpsdk.NewClientSideConnection(c, clientToAgentW, agentToClientR)
	t.Cleanup(func() {
		_ = agentToClientW.Close()
		_ = clientToAgentW.Close()
	})
	return c, func() { _ = agentToClientW.Close() }
}

func delta(i int) ports.ChatEvent {
	return ports.ChatEvent{Kind: ports.ChatEventReasoningDelta, ProviderItemID: "item", ProviderEventID: fmt.Sprintf("e%d", i)}
}

// emitAll emits n events and fails if emitting them blocks: emit runs on the
// SDK's notification goroutine, which must never wait on the consumer.
func emitAll(t *testing.T, c *conversation, from, n int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := from; i < from+n; i++ {
			c.emit(delta(i))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("emitting %d events behind a stalled consumer blocked", n)
	}
}

// 2026-11-13: the consumer's SQLite writer stalled for minutes on a saturated
// disk, a blocking journaled emit stopped the SDK's notification goroutine, the
// SDK's 1,024-slot queue filled and it closed the agent's connection mid
// session/load. Journaled events now wait in memory, in order, instead.
func TestJournaledEventsSpillInOrderInsteadOfBlockingTheSDK(t *testing.T) {
	c, _ := spillConversation(t, 2)
	emitAll(t, c, 0, 500)
	for i := 0; i < 500; i++ {
		select {
		case got := <-c.events:
			if got.ProviderEventID != delta(i).ProviderEventID {
				t.Fatalf("event %d = %q, want %q (order)", i, got.ProviderEventID, delta(i).ProviderEventID)
			}
			if i == 250 {
				// Emits while the pump is still delivering keep their place.
				emitAll(t, c, 500, 50)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d never arrived", i)
		}
	}
	for i := 500; i < 550; i++ {
		got := <-c.events
		if got.ProviderEventID != delta(i).ProviderEventID {
			t.Fatalf("event %d = %q, want %q (order)", i, got.ProviderEventID, delta(i).ProviderEventID)
		}
	}
	waitFor(t, func() bool {
		c.spillMu.Lock()
		defer c.spillMu.Unlock()
		return !c.spilling && len(c.spill) == 0
	}, "the pump to finish once the spill is delivered")
	emitAll(t, c, 550, 1)
	if got := <-c.events; got.ProviderEventID != delta(550).ProviderEventID {
		t.Fatalf("after the spill drained, event = %q, want it delivered directly", got.ProviderEventID)
	}
}

// Past the limit emit waits for room, as it always did, and a closed connection
// releases it and the pump (the host replays unacknowledged frames).
func TestJournaledSpillIsBoundedAndEndsWithTheConnection(t *testing.T) {
	saved := eventSpillLimit
	eventSpillLimit = 8
	t.Cleanup(func() { eventSpillLimit = saved })
	c, closeConn := spillConversation(t, 1)
	emitAll(t, c, 0, 1+8) // one in the channel, eight spilled

	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		c.emit(delta(9))
	}()
	select {
	case <-blocked:
		t.Fatal("an emit past the spill limit did not wait for room")
	case <-time.After(100 * time.Millisecond):
	}
	<-c.events // the pump moves one spilled event in, freeing room
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("an emit waiting at the limit was not woken when room freed")
	}

	closeConn()
	waitFor(t, func() bool {
		c.spillMu.Lock()
		defer c.spillMu.Unlock()
		return !c.spilling && c.spill == nil
	}, "the pump to stop with the connection")
	emitAll(t, c, 20, 30) // a closed connection never blocks emit
}

func waitFor(t *testing.T, ok func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
