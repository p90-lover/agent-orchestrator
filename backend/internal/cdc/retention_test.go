package cdc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeRetentionStore struct {
	ageRows  []int64
	sizeRows []int64
	ageErr   error
	sizeErr  error
	ages     int
	sizes    int
}

func (s *fakeRetentionStore) PruneChangeLogBefore(context.Context, time.Time, int64) (int64, error) {
	s.ages++
	if s.ageErr != nil {
		return 0, s.ageErr
	}
	return nextRetentionResult(&s.ageRows), nil
}

func (s *fakeRetentionStore) PruneChangeLogToMaxRows(context.Context, int64, int64) (int64, error) {
	s.sizes++
	if s.sizeErr != nil {
		return 0, s.sizeErr
	}
	return nextRetentionResult(&s.sizeRows), nil
}

func nextRetentionResult(rows *[]int64) int64 {
	if len(*rows) == 0 {
		return 0
	}
	n := (*rows)[0]
	*rows = (*rows)[1:]
	return n
}

func TestRetentionJanitorRunOncePrunesAgeAndSizeUntilStable(t *testing.T) {
	store := &fakeRetentionStore{
		ageRows:  []int64{2, 1, 0},
		sizeRows: []int64{3, 0},
	}
	j := NewRetentionJanitor(store, RetentionConfig{
		Retention: time.Hour,
		MaxRows:   10,
		Batch:     100,
		Pause:     -1,
		Clock:     func() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err := j.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.ages != 3 || store.sizes != 3 {
		t.Fatalf("prune calls = age %d/size %d, want 3/3", store.ages, store.sizes)
	}
}

// One 10k-row delete held the only writer for over two minutes on a busy hard
// disk (2026-11-11), so every chat call queued behind it timed out.
func TestRetentionJanitorDefaultsKeepEachWriterHoldShort(t *testing.T) {
	j := NewRetentionJanitor(&fakeRetentionStore{}, RetentionConfig{})
	if j.batch != DefaultRetentionBatch || DefaultRetentionBatch > 1_000 {
		t.Fatalf("batch = %d (default %d), want at most 1000 rows per delete", j.batch, DefaultRetentionBatch)
	}
	if j.pause != DefaultRetentionPause || j.pause <= 0 {
		t.Fatalf("pause = %v, want a pause between batches", j.pause)
	}
	if got := int64(j.maxBatches) * j.batch; got != 80_000 {
		t.Fatalf("one run's ceiling = %d events, want 80000 as before", got)
	}
	if NewRetentionJanitor(&fakeRetentionStore{}, RetentionConfig{Pause: -1}).pause >= 0 {
		t.Fatal("a negative pause means none")
	}
}

func TestRetentionJanitorPausesBetweenBatchesAndStopsOnCancel(t *testing.T) {
	store := &fakeRetentionStore{ageRows: []int64{5, 5, 5}}
	j := NewRetentionJanitor(store, RetentionConfig{Batch: 5, Pause: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
	if store.ages != 1 || store.sizes != 1 {
		t.Fatalf("prune calls = age %d/size %d, want 1/1 (it was pausing before the second batch)", store.ages, store.sizes)
	}
}

func TestRetentionJanitorRunOnceReturnsStoreError(t *testing.T) {
	want := errors.New("busy")
	store := &fakeRetentionStore{ageErr: want}
	j := NewRetentionJanitor(store, RetentionConfig{Logger: slog.Default()})
	if err := j.RunOnce(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
