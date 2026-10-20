package cdc

import (
	"context"
	"log/slog"
	"time"
)

const (
	// DefaultProviderDeltaSettled is how long a settled conversation keeps its
	// streaming chunks: long enough to inspect what the provider just sent.
	DefaultProviderDeltaSettled = 6 * time.Hour
	// DefaultProviderDeltaMaxAge bounds chunks of a conversation whose turn is
	// still open (a stuck turn kept them forever before).
	DefaultProviderDeltaMaxAge = 24 * time.Hour
)

// ProviderRetentionStore is the storage surface of the provider-chunk janitor.
type ProviderRetentionStore interface {
	PruneSettledProviderDeltas(ctx context.Context, settledBefore, anyBefore time.Time, limit int64) (int64, error)
}

// ProviderRetentionConfig customizes provider-chunk cleanup; zero values use
// the production defaults.
type ProviderRetentionConfig struct {
	Settled  time.Duration
	MaxAge   time.Duration
	Interval time.Duration
	Batch    int64
	Clock    func() time.Time
	Logger   *slog.Logger
}

// ProviderRetentionJanitor removes streaming chunks (reasoning, message and
// command-output deltas) from conversation_provider_events once nothing can
// replay them. One busy reasoning chat archived over a million chunk rows a
// day; the table grew to 1.2 GB of a 1.7 GB ao.db, and every chunk write
// maintained three indexes. Batches stay small so the single writer is never
// held for long, as with the change-log janitor.
type ProviderRetentionJanitor struct {
	store      ProviderRetentionStore
	settled    time.Duration
	maxAge     time.Duration
	interval   time.Duration
	batch      int64
	clock      func() time.Time
	logger     *slog.Logger
	maxBatches int
}

// NewProviderRetentionJanitor constructs the provider-chunk cleanup worker.
func NewProviderRetentionJanitor(store ProviderRetentionStore, cfg ProviderRetentionConfig) *ProviderRetentionJanitor {
	j := &ProviderRetentionJanitor{
		store: store, settled: cfg.Settled, maxAge: cfg.MaxAge, interval: cfg.Interval,
		batch: cfg.Batch, clock: cfg.Clock, logger: cfg.Logger,
		// A backlog of a million rows clears over a few runs, not in one long burst.
		maxBatches: 20,
	}
	if j.settled <= 0 {
		j.settled = DefaultProviderDeltaSettled
	}
	if j.maxAge <= 0 {
		j.maxAge = DefaultProviderDeltaMaxAge
	}
	if j.interval <= 0 {
		j.interval = DefaultRetentionInterval
	}
	if j.batch <= 0 {
		j.batch = DefaultRetentionBatch
	}
	if j.clock == nil {
		j.clock = time.Now
	}
	if j.logger == nil {
		j.logger = slog.Default()
	}
	return j
}

// Start runs cleanup now and then at the interval until ctx is cancelled; the
// returned channel closes when the worker has stopped.
func (j *ProviderRetentionJanitor) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		j.run(ctx)
		t := time.NewTicker(j.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				j.run(ctx)
			}
		}
	}()
	return done
}

// RunOnce performs one bounded cleanup pass.
func (j *ProviderRetentionJanitor) RunOnce(ctx context.Context) error {
	now := j.clock().UTC()
	var total int64
	for i := 0; i < j.maxBatches; i++ {
		removed, err := j.store.PruneSettledProviderDeltas(ctx, now.Add(-j.settled), now.Add(-j.maxAge), j.batch)
		if err != nil {
			return err
		}
		total += removed
		if removed < j.batch {
			break
		}
	}
	if total > 0 {
		j.logger.Info("provider event retention pruned streaming chunks", "rows", total)
	}
	return nil
}

func (j *ProviderRetentionJanitor) run(ctx context.Context) {
	if err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
		j.logger.Warn("provider event retention failed", "err", err)
	}
}
