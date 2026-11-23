package cdc

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type turnFloorPass struct {
	done bool
	err  error
}

// fakeTurnFloorStore answers each pass with the next of passes, and with done
// once none is left.
type fakeTurnFloorStore struct {
	mu     sync.Mutex
	passes []turnFloorPass
	calls  int
}

func (f *fakeTurnFloorStore) BackfillTurnFloors(context.Context, *slog.Logger) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.passes) == 0 {
		return true, nil
	}
	pass := f.passes[0]
	f.passes = f.passes[1:]
	return pass.done, pass.err
}

func (f *fakeTurnFloorStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func waitStopped(t *testing.T, done <-chan struct{}, why string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(why)
	}
}

// The backfill reads every chat's rows, so it waits until AO has started and
// the retention janitors have made their first passes.
func TestTurnFloorBackfillWaitsOutItsStartDelay(t *testing.T) {
	if got := NewTurnFloorBackfill(&fakeTurnFloorStore{}, TurnFloorBackfillConfig{}).startDelay; got != DefaultTurnFloorStartDelay || got <= DefaultRetentionStartDelay {
		t.Fatalf("start delay = %v, want %v, after the janitors' %v", got, DefaultTurnFloorStartDelay, DefaultRetentionStartDelay)
	}
	store := &fakeTurnFloorStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := NewTurnFloorBackfill(store, TurnFloorBackfillConfig{StartDelay: time.Hour}).Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	waitStopped(t, done, "a backfill cancelled during its start delay must stop")
	if got := store.callCount(); got != 0 {
		t.Fatalf("passes = %d, want none before the start delay ends", got)
	}
}

// A pass that gave the writer back or failed is tried again later; once a pass
// reports every turn done, the worker stops for good: new turns record their
// floor on insert.
func TestTurnFloorBackfillRetriesUntilEveryTurnIsDoneThenStops(t *testing.T) {
	store := &fakeTurnFloorStore{passes: []turnFloorPass{
		{done: false},
		{err: errors.New("disk I/O error")},
		{done: false},
	}}
	done := NewTurnFloorBackfill(store, TurnFloorBackfillConfig{
		StartDelay: -1, Retry: time.Millisecond, RetryAfterError: time.Millisecond,
		Logger: slog.New(slog.DiscardHandler),
	}).Start(context.Background())
	waitStopped(t, done, "the backfill kept running after a pass reported every turn done")
	if got := store.callCount(); got != 4 {
		t.Fatalf("passes = %d, want 3 unfinished and the 1 that finished", got)
	}
}

func TestTurnFloorBackfillStopsOnCancelBetweenPasses(t *testing.T) {
	store := &fakeTurnFloorStore{passes: []turnFloorPass{{done: false}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := NewTurnFloorBackfill(store, TurnFloorBackfillConfig{StartDelay: -1, Retry: time.Hour}).Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for store.callCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first pass never ran")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	waitStopped(t, done, "a cancelled backfill must not wait out its retry")
	if got := store.callCount(); got != 1 {
		t.Fatalf("passes = %d, want 1", got)
	}
}
