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
	// DefaultProviderDeltaBatch keeps each delete short. On a real 1.6 GB copy,
	// 10k-row batches took 1.2 s on average and 14.6 s at worst, holding the
	// single writer (and every chat's writes) that long.
	DefaultProviderDeltaBatch int64 = 2_000
	// DefaultProviderDeltaPause lets chat writes take the writer between batches.
	DefaultProviderDeltaPause = 250 * time.Millisecond
	// defaultProviderDeltaBatches bounds one run: 200k rows, so a backlog of a
	// million chunk rows clears in a few runs.
	defaultProviderDeltaBatches = 100
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
	// Pause between batches; negative means none (tests).
	Pause time.Duration
	// StartDelay before the first pass (DefaultRetentionStartDelay); negative means none.
	StartDelay time.Duration
	Clock      func() time.Time
	Logger     *slog.Logger
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
	pause      time.Duration
	startDelay time.Duration
	clock      func() time.Time
	logger     *slog.Logger
	maxBatches int
}

// NewProviderRetentionJanitor constructs the provider-chunk cleanup worker.
func NewProviderRetentionJanitor(store ProviderRetentionStore, cfg ProviderRetentionConfig) *ProviderRetentionJanitor {
	j := &ProviderRetentionJanitor{
		store: store, settled: cfg.Settled, maxAge: cfg.MaxAge, interval: cfg.Interval,
		batch: cfg.Batch, pause: cfg.Pause, startDelay: cfg.StartDelay, clock: cfg.Clock, logger: cfg.Logger,
		maxBatches: defaultProviderDeltaBatches,
	}
	if j.pause == 0 {
		j.pause = DefaultProviderDeltaPause
	}
	if j.startDelay == 0 {
		j.startDelay = DefaultRetentionStartDelay
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
		j.batch = DefaultProviderDeltaBatch
	}
	if j.clock == nil {
		j.clock = time.Now
	}
	if j.logger == nil {
		j.logger = slog.Default()
	}
	return j
}

// Start runs cleanup after the start delay and then at the interval until ctx
// is cancelled; the returned channel closes when the worker has stopped.
func (j *ProviderRetentionJanitor) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !waitStartDelay(ctx, j.startDelay) {
			return
		}
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
		if j.pause > 0 {
			select {
			case <-ctx.Done():
				j.logPruned(total)
				return ctx.Err()
			case <-time.After(j.pause):
			}
		}
	}
	j.logPruned(total)
	return nil
}

func (j *ProviderRetentionJanitor) logPruned(total int64) {
	if total > 0 {
		j.logger.Info("provider event retention pruned streaming chunks", "rows", total)
	}
}

func (j *ProviderRetentionJanitor) run(ctx context.Context) {
	if err := j.RunOnce(ctx); err != nil && ctx.Err() == nil {
		j.logger.Warn("provider event retention failed", "err", err)
	}
}
