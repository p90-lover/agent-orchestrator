package cdc

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeProviderStore struct {
	calls []struct {
		settled, any time.Time
		limit        int64
	}
	results []int64
	err     error
}

func (f *fakeProviderStore) PruneSettledProviderDeltas(_ context.Context, settled, anyBefore time.Time, limit int64) (int64, error) {
	f.calls = append(f.calls, struct {
		settled, any time.Time
		limit        int64
	}{settled, anyBefore, limit})
	if f.err != nil {
		return 0, f.err
	}
	if len(f.results) == 0 {
		return 0, nil
	}
	n := f.results[0]
	f.results = f.results[1:]
	return n, nil
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
