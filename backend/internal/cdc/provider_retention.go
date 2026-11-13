package cdc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
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
	// PruneSettledProviderDeltas deletes up to limit matching rows with an id in
	// (afterID, throughID]; found and lastID say how far the read got.
	PruneSettledProviderDeltas(ctx context.Context, settledBefore, anyBefore time.Time, afterID, throughID, limit int64) (found, removed, lastID int64, err error)
	// ProviderEventIDBefore returns the highest id up to which every event after
	// afterID was received before cutoff.
	ProviderEventIDBefore(ctx context.Context, afterID int64, cutoff time.Time) (int64, error)
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
	// size is the delete size the store can take without holding its writer
	// away from chat writes; batch is the most it grows back to.
	size yieldingBatch

	// The archive is walked in id order by sweeps, never reread from its first
	// row. Below floor every event is older than maxAge was when the sweep that
	// set it began, and every chunk row there was deletable outright, so it is
	// gone; what stays (turn, item, approval and usage events) is never deleted
	// here, so no later sweep needs to read it. Only RunOnce touches these.
	floor int64
	sweep providerSweep
}

// providerSweep is a walk of the archive from floor, possibly over several
// passes: a pass that yields the writer, fails or hits maxBatches continues it
// next time instead of starting over.
type providerSweep struct {
	// start is when the sweep began; zero means no sweep is in progress.
	start time.Time
	// next is the id the next batch reads after.
	next int64
	// through is the last id received before settled was at start. Rows after
	// it were too young to match then; the next sweep reads them.
	through int64
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
	j.size = newYieldingBatch(j.batch)
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
		for {
			// A pass that gave the writer back to a chat write comes back sooner.
			t := time.NewTimer(j.size.wait(j.interval))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				j.run(ctx)
			}
		}
	}()
	return done
}

// RunOnce performs one bounded cleanup pass, continuing the current sweep or
// starting one at floor.
func (j *ProviderRetentionJanitor) RunOnce(ctx context.Context) error {
	now := j.clock().UTC()
	if j.sweep.start.IsZero() {
		through, err := j.store.ProviderEventIDBefore(ctx, j.floor, now.Add(-j.settled))
		if err != nil {
			return err
		}
		j.sweep = providerSweep{start: now, next: j.floor, through: through}
	}
	var total int64
	for i := 0; i < j.maxBatches; i++ {
		size := j.size.size
		found, removed, last, err := j.store.PruneSettledProviderDeltas(ctx, now.Add(-j.settled), now.Add(-j.maxAge), j.sweep.next, j.sweep.through, size)
		// A chat write needed the writer: stop here, and come back soon with a
		// smaller delete. Nothing failed, so this is not reported as an error.
		// The sweep stays where it was, so the next pass reads these rows again.
		if errors.Is(err, ports.ErrMaintenanceYielded) {
			j.size.yielded()
			j.logPruned(total)
			j.logger.Info("provider event retention gave the writer to a waiting write", "nextBatch", j.size.size)
			return nil
		}
		if err != nil {
			return err
		}
		total += removed
		if found < size {
			if err := j.finishSweep(ctx); err != nil {
				return err
			}
			break
		}
		j.sweep.next = last
		if j.pause > 0 {
			select {
			case <-ctx.Done():
				j.logPruned(total)
				return ctx.Err()
			case <-time.After(j.pause):
			}
		}
	}
	j.size.finished()
	j.logPruned(total)
	return nil
}

// finishSweep raises floor once a sweep has read through its range. Every chunk
// row received before maxAge was at the sweep's start matched its batch
// unconditionally, so each one up to the first younger row was deleted. Rows
// kept for an open turn are younger than that, so they stay above floor and
// the next sweep reads them again. Floor never passes through: rows after it
// were not read.
func (j *ProviderRetentionJanitor) finishSweep(ctx context.Context) error {
	sweep := j.sweep
	j.sweep = providerSweep{}
	floor, err := j.store.ProviderEventIDBefore(ctx, j.floor, sweep.start.Add(-j.maxAge))
	if err != nil {
		return err
	}
	j.floor = max(j.floor, min(floor, sweep.through))
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
