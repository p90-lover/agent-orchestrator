package cdc

import (
	"context"
	"log/slog"
	"time"
)

// DefaultTurnFloorStartDelay puts the turn-floor backfill after the retention
// janitors' first passes (DefaultRetentionStartDelay), so the two reads do not
// meet on the disk.
const DefaultTurnFloorStartDelay = 2 * DefaultRetentionStartDelay

// TurnFloorStore is the storage surface of the turn-floor backfill.
type TurnFloorStore interface {
	// BackfillTurnFloors gives turns written before migration 9011 their
	// floor_sequence and reports whether every turn has one now. false with no
	// error means it gave the writer back before it finished.
	BackfillTurnFloors(ctx context.Context, logger *slog.Logger) (bool, error)
}

// TurnFloorBackfillConfig customizes the backfill; zero values use the
// production defaults.
type TurnFloorBackfillConfig struct {
	// StartDelay before the first pass (DefaultTurnFloorStartDelay); negative
	// means none.
	StartDelay time.Duration
	// Retry after a pass that gave the writer back (yieldRetry), and after one
	// that failed (DefaultRetentionInterval).
	Retry, RetryAfterError time.Duration
	Logger                 *slog.Logger
}

// TurnFloorBackfill fills conversation_turns.floor_sequence for the turns
// written before migration 9011, once, after AO is listening: the store reads
// every chat's rows to do it, which a migration would do before AO answers.
// New turns record their floor on insert, so once a pass reports every turn
// done the worker stops for good.
type TurnFloorBackfill struct {
	store           TurnFloorStore
	startDelay      time.Duration
	retry           time.Duration
	retryAfterError time.Duration
	logger          *slog.Logger
}

// NewTurnFloorBackfill constructs the backfill worker.
func NewTurnFloorBackfill(store TurnFloorStore, cfg TurnFloorBackfillConfig) *TurnFloorBackfill {
	b := &TurnFloorBackfill{
		store: store, startDelay: cfg.StartDelay, retry: cfg.Retry,
		retryAfterError: cfg.RetryAfterError, logger: cfg.Logger,
	}
	if b.startDelay == 0 {
		b.startDelay = DefaultTurnFloorStartDelay
	}
	if b.retry <= 0 {
		b.retry = yieldRetry
	}
	if b.retryAfterError <= 0 {
		b.retryAfterError = DefaultRetentionInterval
	}
	if b.logger == nil {
		b.logger = slog.Default()
	}
	return b
}

// Start runs passes after the start delay until one reports every turn done
// or ctx is cancelled; the returned channel closes when the worker has
// stopped.
func (b *TurnFloorBackfill) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !waitStartDelay(ctx, b.startDelay) {
			return
		}
		for {
			finished, err := b.store.BackfillTurnFloors(ctx, b.logger)
			if finished {
				return
			}
			wait := b.retry
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				b.logger.Warn("turn floor backfill failed", "err", err)
				wait = b.retryAfterError
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
	return done
}
