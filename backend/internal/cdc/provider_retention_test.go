package cdc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type providerPruneCall struct {
	settled, any          time.Time
	after, through, limit int64
}

type providerBeforeCall struct {
	after  int64
	cutoff time.Time
}

// fakeProviderStore answers each batch with the next result as the rows found
// (ids after afterID, one apart) and each ProviderEventIDBefore with the next
// of before, or afterID+1000 when none is left.
type fakeProviderStore struct {
	calls   []providerPruneCall
	results []int64
	asked   []providerBeforeCall
	before  []int64
	// yieldCall makes that batch (1-based) yield the writer.
	yieldCall int
	err       error
}

func (f *fakeProviderStore) PruneSettledProviderDeltas(_ context.Context, settled, anyBefore time.Time, after, through, limit int64) (int64, int64, int64, error) {
	f.calls = append(f.calls, providerPruneCall{settled, anyBefore, after, through, limit})
	if f.err != nil {
		return 0, 0, 0, f.err
	}
	if len(f.calls) == f.yieldCall {
		return 0, 0, 0, ports.ErrMaintenanceYielded
	}
	if len(f.results) == 0 {
		return 0, 0, 0, nil
	}
	n := f.results[0]
	f.results = f.results[1:]
	return n, n, after + n, nil
}

func (f *fakeProviderStore) ProviderEventIDBefore(_ context.Context, after int64, cutoff time.Time) (int64, error) {
	f.asked = append(f.asked, providerBeforeCall{after, cutoff})
	if f.err != nil {
		return 0, f.err
	}
	if len(f.before) == 0 {
		return after + 1000, nil
	}
	id := f.before[0]
	f.before = f.before[1:]
	return id, nil
}

// A sweep reads the archive once: each batch starts after the last row the one
// before it matched, inside the range that was old enough at the sweep's start.
// When it reaches the end, floor rises to where every row was past maxAge, and
// the next sweep starts there instead of at the first row (2026-11-13: rereading
// a 3.9 GB database from its first row on every batch and every pass was about
// 300 GB of reads in twelve hours on a shared HDD).
func TestProviderRetentionSweepsResumeAndRaiseTheirFloor(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clock := now
	store := &fakeProviderStore{results: []int64{3, 3, 1, 0}, before: []int64{500, 120, 900, 950}}
	j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 3, Pause: -1, Clock: func() time.Time { return clock }})
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []providerPruneCall{
		{now.Add(-DefaultProviderDeltaSettled), now.Add(-DefaultProviderDeltaMaxAge), 0, 500, 3},
		{now.Add(-DefaultProviderDeltaSettled), now.Add(-DefaultProviderDeltaMaxAge), 3, 500, 3},
		{now.Add(-DefaultProviderDeltaSettled), now.Add(-DefaultProviderDeltaMaxAge), 6, 500, 3},
	}
	if len(store.calls) != len(want) {
		t.Fatalf("batches = %+v, want %+v", store.calls, want)
	}
	for i := range want {
		if store.calls[i] != want[i] {
			t.Fatalf("batch %d = %+v, want %+v", i, store.calls[i], want[i])
		}
	}
	if len(store.asked) != 2 || store.asked[0] != (providerBeforeCall{0, now.Add(-DefaultProviderDeltaSettled)}) ||
		store.asked[1] != (providerBeforeCall{0, now.Add(-DefaultProviderDeltaMaxAge)}) {
		t.Fatalf("range questions = %+v, want the settled cutoff from 0, then the max-age one", store.asked)
	}
	if j.floor != 120 || !j.sweep.start.IsZero() {
		t.Fatalf("floor = %d, sweep = %+v; want 120 and no sweep in progress", j.floor, j.sweep)
	}

	// The next sweep starts at floor and, even when the store would vouch for
	// more, never raises floor past the range it read.
	clock = now.Add(15 * time.Minute)
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := store.calls[len(store.calls)-1]; last.after != 120 || last.through != 900 {
		t.Fatalf("second sweep's batch = %+v, want (120, 900]", last)
	}
	if store.asked[2].after != 120 || j.floor != 900 {
		t.Fatalf("second sweep asked from %d and set floor %d; want 120 and 900 (its through, not the store's 950)", store.asked[2].after, j.floor)
	}
}

// A pass that gave the writer away, failed or hit its batch cap continues its
// sweep where it was; it neither starts over at floor nor moves floor.
func TestProviderRetentionSweepContinuesAfterAYield(t *testing.T) {
	store := &fakeProviderStore{results: []int64{2, 1}, before: []int64{50, 40}, yieldCall: 2}
	j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 2, Pause: -1, Logger: quiet})
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if j.floor != 0 || j.sweep.next != 2 || j.sweep.through != 50 {
		t.Fatalf("after the yield floor = %d sweep = %+v; want floor 0, next 2, through 50", j.floor, j.sweep)
	}
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := store.calls[len(store.calls)-1]; last.after != 2 || last.through != 50 {
		t.Fatalf("resumed batch = %+v, want (2, 50]", last)
	}
	if len(store.asked) != 2 || j.floor != 40 {
		t.Fatalf("range questions = %d, floor = %d; want 2 (no new sweep range) and 40", len(store.asked), j.floor)
	}
}

func TestProviderRetentionJanitorPrunesInBatchesWithBothCutoffs(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeProviderStore{results: []int64{3, 3, 1}}
	j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 3, Pause: -1, Clock: func() time.Time { return now }})
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != 3 {
		t.Fatalf("calls = %d, want 3 (stops after a short batch)", len(store.calls))
	}
	first := store.calls[0]
	if !first.settled.Equal(now.Add(-DefaultProviderDeltaSettled)) || !first.any.Equal(now.Add(-DefaultProviderDeltaMaxAge)) || first.limit != 3 {
		t.Fatalf("first call = %+v, want settled -6h, any -24h, limit 3", first)
	}
}

func TestProviderRetentionJanitorBoundsOneRunAndReportsErrors(t *testing.T) {
	full := make([]int64, 3*defaultProviderDeltaBatches)
	for i := range full {
		full[i] = 2
	}
	store := &fakeProviderStore{results: full}
	j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 2, Pause: -1})
	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.calls) != j.maxBatches {
		t.Fatalf("calls = %d, want the per-run cap %d", len(store.calls), j.maxBatches)
	}
	failing := &fakeProviderStore{err: errors.New("busy")}
	if err := NewProviderRetentionJanitor(failing, ProviderRetentionConfig{}).RunOnce(context.Background()); err == nil {
		t.Fatal("want the store error")
	}
}

// Right after a reboot the first pass read the cold archive for over a minute at
// startup and kept AO from getting ready; it now waits out a start delay.
func TestRetentionJanitorsWaitOutTheirStartDelay(t *testing.T) {
	if got := NewProviderRetentionJanitor(&fakeProviderStore{}, ProviderRetentionConfig{}).startDelay; got != DefaultRetentionStartDelay {
		t.Fatalf("provider janitor start delay = %v, want %v", got, DefaultRetentionStartDelay)
	}
	if got := NewRetentionJanitor(&fakeRetentionStore{}, RetentionConfig{}).startDelay; got != DefaultRetentionStartDelay {
		t.Fatalf("change-log janitor start delay = %v, want %v", got, DefaultRetentionStartDelay)
	}

	waiting := &fakeProviderStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := NewProviderRetentionJanitor(waiting, ProviderRetentionConfig{StartDelay: time.Hour, Pause: -1}).Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a janitor cancelled during its start delay must stop")
	}
	if len(waiting.calls) != 0 {
		t.Fatalf("calls = %d, want none before the start delay ends", len(waiting.calls))
	}

	immediate := &fakeProviderStore{}
	ctx, cancel = context.WithCancel(context.Background())
	done = NewProviderRetentionJanitor(immediate, ProviderRetentionConfig{StartDelay: -1, Pause: -1}).Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if len(immediate.calls) != 1 {
		t.Fatalf("calls = %d, want the first pass at once with no start delay", len(immediate.calls))
	}
}

func TestProviderRetentionJanitorPausesBetweenBatchesAndStopsOnCancel(t *testing.T) {
	store := &fakeProviderStore{results: []int64{2, 2, 2}}
	j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 2, Pause: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- j.RunOnce(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled run must not wait out its pause")
	}
	if len(store.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (it was pausing before the second batch)", len(store.calls))
	}
}
