package store_test

import (
	"context"
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
