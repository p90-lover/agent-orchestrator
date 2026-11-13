package store_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The janitor walks the archive by id range instead of rereading it from its
// first row: a batch reads and deletes only inside (afterID, throughID], says
// how far it got, and ProviderEventIDBefore vouches only for the ids before the
// first row received at or after a cutoff, even when an older row comes later.
func TestProviderDeltaPruneStaysInsideItsIDRange(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	noop := func(context.Context) error { return nil }
	now := histClock.Add(48 * time.Hour)
	archive := func(method string, at time.Time) {
		t.Helper()
		if _, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "", method, `{"delta":"x"}`, at, noop); err != nil {
			t.Fatalf("archive %s: %v", method, err)
		}
	}
	archive("reasoning.delta", now.Add(-30*time.Hour)) // old: deletable outright
	archive("turn.started", now.Add(-30*time.Hour))    // never deleted here
	archive("message.delta", now.Add(-10*time.Hour))   // settled: deletable
	archive("message.delta", now.Add(-time.Minute))    // too young
	archive("reasoning.delta", now.Add(-30*time.Hour)) // old, but archived last
	events, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil || len(events) != 5 {
		t.Fatalf("archive = %d rows, %v; want 5", len(events), err)
	}
	id := func(i int) int64 { return events[i].ID }

	for _, check := range []struct {
		after  int64
		cutoff time.Time
		want   int64
	}{
		{0, now.Add(-24 * time.Hour), id(1)},         // stops before the -10h row, not at the last old one
		{0, now.Add(-6 * time.Hour), id(2)},          // stops before the -1m row
		{id(2), now.Add(time.Hour), id(4)},           // nothing younger: the newest id
		{id(4), now.Add(time.Hour), id(4)},           // nothing after afterID: afterID
		{id(4) + 10, now.Add(time.Hour), id(4) + 10}, // never moves back
	} {
		got, err := s.ProviderEventIDBefore(ctx, check.after, check.cutoff)
		if err != nil || got != check.want {
			t.Fatalf("ProviderEventIDBefore(%d, %s) = %d, %v; want %d", check.after, check.cutoff, got, err, check.want)
		}
	}

	settledBefore, anyBefore := now.Add(-6*time.Hour), now.Add(-24*time.Hour)
	prune := func(after, through, limit int64) (found, removed, last int64) {
		t.Helper()
		found, removed, last, err := s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, after, through, limit)
		if err != nil {
			t.Fatal(err)
		}
		return found, removed, last
	}
	if found, removed, last := prune(0, id(3), 1); found != 1 || removed != 1 || last != id(0) {
		t.Fatalf("first batch = found %d removed %d last %d; want 1, 1, %d", found, removed, last, id(0))
	}
	if found, removed, last := prune(id(0), id(3), 100); found != 1 || removed != 1 || last != id(2) {
		t.Fatalf("second batch = found %d removed %d last %d; want 1, 1, %d (the old row after through stays)", found, removed, last, id(2))
	}
	if found, _, _ := prune(id(2), id(2), 100); found != 0 {
		t.Fatalf("an empty range found %d rows", found)
	}
	if found, removed, last := prune(id(3), id(4), 100); found != 1 || removed != 1 || last != id(4) {
		t.Fatalf("the last range = found %d removed %d last %d; want 1, 1, %d", found, removed, last, id(4))
	}
	left, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil || len(left) != 2 || left[0].Method != "turn.started" || left[1].ID != id(3) {
		t.Fatalf("remaining = %d rows, %v; want the turn event and the young delta", len(left), err)
	}
}

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
	_, removed, _, err := s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 0, math.MaxInt64, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("while the turn runs removed = %d, want 1 (only the delta past the hard limit)", removed)
	}

	if err := s.SettleTurn(ctx, conversation, "provider-turn-1", domain.TurnStateCompleted, "", now.Add(-9*time.Hour)); err != nil {
		t.Fatalf("settle turn: %v", err)
	}
	_, removed, _, err = s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 0, math.MaxInt64, 1)
	if err != nil || removed != 1 {
		t.Fatalf("first batch after settling removed = %d, %v; want 1 (the batch limit)", removed, err)
	}
	_, removed, _, err = s.PruneSettledProviderDeltas(ctx, settledBefore, anyBefore, 0, math.MaxInt64, 100)
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
