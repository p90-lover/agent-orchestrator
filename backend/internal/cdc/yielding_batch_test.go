package cdc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// sizedStore records the delete size each janitor pass asked for, and yields
// (as the store does when a chat write waits) while yields remain.
type sizedStore struct {
	yields int
	limits []int64
}

func (s *sizedStore) prune(limit int64) (int64, error) {
	s.limits = append(s.limits, limit)
	if s.yields > 0 {
		s.yields--
		return 0, ports.ErrMaintenanceYielded
	}
	return 0, nil
}

func (s *sizedStore) PruneChangeLogBefore(_ context.Context, _ time.Time, limit int64) (int64, error) {
	return s.prune(limit)
}

func (s *sizedStore) PruneChangeLogToMaxRows(context.Context, int64, int64) (int64, error) {
	return 0, nil
}

func (s *sizedStore) PruneSettledProviderDeltas(_ context.Context, _, _ time.Time, limit int64) (int64, error) {
	return s.prune(limit)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// A delete that had to give the writer to a chat write is not an error: the
// janitor shrinks its next delete, comes back within a minute instead of its
// 15-minute interval, and grows back once deletes finish again.
func TestJanitorsShrinkAndRetrySoonAfterYieldingThenGrowBack(t *testing.T) {
	passes := map[string]func(*sizedStore) (func() error, *yieldingBatch){
		"change log": func(store *sizedStore) (func() error, *yieldingBatch) {
			j := NewRetentionJanitor(store, RetentionConfig{Batch: 1600, Logger: quiet})
			return func() error { return j.RunOnce(context.Background()) }, &j.size
		},
		"provider chunks": func(store *sizedStore) (func() error, *yieldingBatch) {
			j := NewProviderRetentionJanitor(store, ProviderRetentionConfig{Batch: 1600, Pause: -1, Logger: quiet})
			return func() error { return j.RunOnce(context.Background()) }, &j.size
		},
	}
	for name, build := range passes {
		t.Run(name, func(t *testing.T) {
			store := &sizedStore{yields: 3}
			run, size := build(store)
			for pass := 0; pass < 6; pass++ {
				if err := run(); err != nil {
					t.Fatalf("pass %d: a yield was reported as a failure: %v", pass, err)
				}
				wantSoon := pass < 3
				if got := size.wait(15 * time.Minute); (got == yieldRetry) != wantSoon {
					t.Fatalf("pass %d: next pass in %s, retry-soon want %v", pass, got, wantSoon)
				}
			}
			// 1600 yields -> 400 yields -> 100 yields -> 100 (the floor) finishes,
			// then doubles back: 200, 400.
			want := []int64{1600, 400, 100, 100, 200, 400}
			if len(store.limits) != len(want) {
				t.Fatalf("delete sizes = %v, want %v", store.limits, want)
			}
			for i := range want {
				if store.limits[i] != want[i] {
					t.Fatalf("delete sizes = %v, want %v", store.limits, want)
				}
			}
		})
	}
}

func TestYieldingBatchNeverLeavesItsBounds(t *testing.T) {
	small := newYieldingBatch(40)
	small.yielded()
	if small.size != 40 {
		t.Fatalf("a batch below the floor shrank to %d, want it kept at 40", small.size)
	}
	big := newYieldingBatch(10_000)
	for range 10 {
		big.yielded()
	}
	if big.size != minYieldingBatch {
		t.Fatalf("size = %d, want the floor %d", big.size, minYieldingBatch)
	}
	for range 10 {
		big.finished()
	}
	if big.size != 10_000 || big.again {
		t.Fatalf("size = %d again = %v, want 10000 and no early retry", big.size, big.again)
	}
	if got := big.wait(30 * time.Second); got != 30*time.Second {
		t.Fatalf("wait = %s, want the interval", got)
	}
}
