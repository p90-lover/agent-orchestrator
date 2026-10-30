package codexappserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver/codexproto"
)

func pushGoal(t *testing.T, srv *scriptedServer, objective, status string, timeUsed int) {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"threadId": "thread-1",
		"turnId": "turn-1",
		"goal": map[string]any{
			"threadId": "thread-1", "objective": objective, "status": status,
			"updatedAt": timeUsed, "createdAt": 1,
			"tokensUsed": 10, "timeUsedSeconds": timeUsed,
		},
	})
	if err != nil { t.Fatal(err) }
	srv.push(`{"method":"thread/goal/updated","params":` + string(params) + `}`)
}

func TestEditedGoalSteersActiveTurnToReconsiderNewObjective(t *testing.T) {
	conv, srv := steerConversation(t)
	// turn/start returning is not an acknowledgement. The real notification
	// establishes that the existing turn can accept steering.
	srv.push(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}}`)
	// Wait for the pump to record the provider's start acknowledgement.
	deadline := time.After(2 * time.Second)
	for {
		conv.mu.Lock()
		started := conv.turnStarted
		conv.mu.Unlock()
		if started { break }
		select { case <-deadline: t.Fatal("turn start was never observed"); default: time.Sleep(time.Millisecond) }
	}
	pushGoal(t, srv, "Finish A first", "active", 1)
	sent := srv.awaitFrame(func(f frame) bool { return f.Method == codexproto.MethodTurnSteer })
	var steer codexproto.TurnSteerParams
	if err := json.Unmarshal(sent.Params, &steer); err != nil { t.Fatal(err) }
	if steer.ExpectedTurnID != "turn-1" || steer.ThreadID != "thread-1" {
		t.Fatalf("steer targeted wrong turn: %+v", steer)
	}
	if len(steer.Input) != 1 || steer.Input[0].Text == nil ||
		!strings.Contains(*steer.Input[0].Text, "Finish A first") ||
		!strings.Contains(*steer.Input[0].Text, "Re-evaluate your plan now") {
		t.Fatalf("steer did not deliver a replan directive: %+v", steer.Input)
	}
	// The original turn stays active: no extra turn/start is issued.
	conv.mu.Lock()
	active := conv.activeTurn
	conv.mu.Unlock()
	if active != "turn-1" { t.Fatalf("changed the running turn: %q", active) }
}

func TestGoalAccountingUpdateDoesNotReSteerOrStartAnIdleTurn(t *testing.T) {
	conv, srv := steerConversation(t)
	pushGoal(t, srv, "Build the UI", "active", 1)
	// Without turn/started, the update must not be forwarded prematurely.
	time.Sleep(20 * time.Millisecond)
	if srv.sentMethod(codexproto.MethodTurnSteer) { t.Fatal("steered before the provider acknowledged the turn") }
	// Make the turn steerable and edit once.
	srv.push(`{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}}`)
	deadline := time.After(2 * time.Second)
	for {
		conv.mu.Lock(); ready := conv.turnStarted; conv.mu.Unlock()
		if ready { break }
		select { case <-deadline: t.Fatal("turn not started"); default: time.Sleep(time.Millisecond) }
	}
	_ = srv.awaitFrame(func(f frame) bool { return f.Method == codexproto.MethodTurnSteer })
	// A goal changed before turn/started was deferred, never lost.
	pushGoal(t, srv, "Build the improved UI", "active", 2)
	deadline = time.After(2 * time.Second)
	for {
		srv.mu.Lock()
		count := 0
		for _, f := range srv.seen { if f.Method == codexproto.MethodTurnSteer { count++ } }
		srv.mu.Unlock()
		if count == 2 { break }
		select { case <-deadline: t.Fatalf("expected two goal steers, got %d", count); default: time.Sleep(time.Millisecond) }
	}
	pushGoal(t, srv, "Build the improved UI", "active", 3)
	time.Sleep(30 * time.Millisecond)
	srv.mu.Lock()
	count := 0
	for _, f := range srv.seen { if f.Method == codexproto.MethodTurnSteer { count++ } }
	srv.mu.Unlock()
	if count != 2 { t.Fatalf("usage-only update sent %d steers, want 2", count) }
}
