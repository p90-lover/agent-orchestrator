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
