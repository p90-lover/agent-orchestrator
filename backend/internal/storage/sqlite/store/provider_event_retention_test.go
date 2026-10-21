package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPruneSettledProviderDeltasKeepsOpenTurnsAndNonDeltaEvents(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	noop := func(context.Context) error { return nil }
	archive := func(method string, at time.Time) {
		t.Helper()
		if _, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "", method, `{"delta":"x"}`, at, noop); err != nil {
			t.Fatalf("archive %s: %v", method, err)
		}
	}
	now := histClock.Add(48 * time.Hour)
	if err := s.AdoptProviderTurn(ctx, conversation, session, "gen-1", "turn-1", "provider-turn-1", now.Add(-10*time.Hour)); err != nil {
		t.Fatalf("adopt turn: %v", err)
	}
	archive("turn.started", now.Add(-10*time.Hour))
	archive("reasoning.delta", now.Add(-10*time.Hour))
	archive("message.delta", now.Add(-10*time.Hour))
	archive("command.output.delta", now.Add(-30*time.Hour))
	archive("reasoning.delta", now.Add(-time.Minute))

	settledBefore, anyBefore := now.Add(-6*time.Hour), now.Add(-24*time.Hour)
	removed, err := s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("while the turn runs removed = %d, want 1 (only the delta past the hard limit)", removed)
	}

	if err := s.SettleTurn(ctx, conversation, "provider-turn-1", domain.TurnStateCompleted, "", now.Add(-9*time.Hour)); err != nil {
		t.Fatalf("settle turn: %v", err)
	}
	removed, err = s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 1)
	if err != nil || removed != 1 {
		t.Fatalf("first batch after settling removed = %d, %v; want 1 (the batch limit)", removed, err)
	}
	removed, err = s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 100)
	if err != nil || removed != 1 {
		t.Fatalf("second batch removed = %d, %v; want the other settled delta", removed, err)
	}

	events, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, e := range events {
		methods = append(methods, e.Method)
	}
	if len(methods) != 2 || methods[0] != "turn.started" || methods[1] != "reasoning.delta" {
		t.Fatalf("remaining = %v, want the turn event and the recent delta", methods)
	}
}

// A persistent host's deltas are deduplicated by the host's position, not by an
// archive row: each applies once in order, a replay of an applied position is
// skipped, and a new host (a new identity) starts its own position.
func TestHostDeltasApplyOnceWithoutArchiveRows(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	applied := 0
	project := func(context.Context) error { applied++; return nil }
	send := func(method, id string) bool {
		t.Helper()
		ok, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", id, method, `{"delta":"x"}`, histClock, project)
		if err != nil {
			t.Fatalf("project %s %s: %v", method, id, err)
		}
		return ok
	}
	steps := []struct {
		method, id string
		want       bool
	}{
		{"reasoning.delta", "acp-host:aaa:5:0", true},
		{"reasoning.delta", "acp-host:aaa:5:1", true},
		{"message.delta", "acp-host:aaa:6:0", true},
		{"reasoning.delta", "acp-host:aaa:5:1", false}, // replayed
		{"message.delta", "acp-host:aaa:6:0", false},   // replayed
		{"reasoning.delta", "acp-host:aaa:10:0", true},
		{"reasoning.delta", "acp-host:bbb:1:0", true}, // a restarted host
		{"turn.started", "acp-host:aaa:7:0", true},    // not a delta: archived
		{"turn.started", "acp-host:aaa:7:0", false},   // and deduplicated by the archive
	}
	for i, step := range steps {
		if got := send(step.method, step.id); got != step.want {
			t.Fatalf("step %d (%s %s) projected = %v, want %v", i, step.method, step.id, got, step.want)
		}
	}
	if applied != 6 {
		t.Fatalf("applied = %d, want 6", applied)
	}
	events, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Method != "turn.started" {
		t.Fatalf("archive = %d rows, want only the turn event", len(events))
	}
}

// A delta rolled back with its batch leaves the position where it was, so the
// provider's replay of it still applies.
func TestHostDeltaMarkRollsBackWithItsBatch(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	failing := errors.New("projection failed")
	err := s.InProviderEventBatch(ctx, func(txCtx context.Context) error {
		if _, err := s.ProjectProviderEvent(txCtx, conversation, session, "gen-1", "acp-host:aaa:1:0",
			"reasoning.delta", `{}`, histClock, func(context.Context) error { return nil }); err != nil {
			return err
		}
		return failing
	})
	if !errors.Is(err, failing) {
		t.Fatalf("batch err = %v", err)
	}
	ok, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "acp-host:aaa:1:0",
		"reasoning.delta", `{}`, histClock, func(context.Context) error { return nil })
	if err != nil || !ok {
		t.Fatalf("replay after rollback projected = %v, %v; want applied", ok, err)
	}
}
