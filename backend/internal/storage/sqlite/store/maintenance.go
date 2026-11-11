package store

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// defaultMaintenanceYieldAfter is how long a background write may hold the
	// writer once a foreground write is waiting for it.
	defaultMaintenanceYieldAfter = 750 * time.Millisecond
	// defaultMaintenanceHoldLimit ends a background write nobody is waiting for
	// yet: the next chat write would otherwise start its wait behind a statement
	// that has already run this long.
	defaultMaintenanceHoldLimit = 20 * time.Second
	maintenancePoll             = 50 * time.Millisecond
)

// maintain runs one background write (retention, purges) under the single
// writer, and gives the writer back when a foreground write needs it.
//
// Every write shares one connection, and cleanup ran on it with no limit: on
// 2026-11-11 one change_log delete held it for over five minutes on a busy HDD
// while eight chat writes (approval answers, messages, session creates) waited,
// and the watcher could only report it. Cleanup is never urgent, so it yields:
// once it has held the writer for the yield time and a write is waiting, on the
// lock or on the connection, or once it has held it for the hold limit, its
// statement is interrupted. SQLite rolls an interrupted statement back, so
// nothing is half-deleted; the caller gets ports.ErrMaintenanceYielded and
// tries again later with a smaller batch.
//
// fn must run its statement with the context it is given, outside any
// transaction it does not itself end.
func (s *Store) maintain(ctx context.Context, fn func(context.Context) (int64, error)) (int64, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.writeMu.Unlock()
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	poolWaits := s.writeDB.Stats().WaitCount
	started := time.Now()
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(maintenancePoll)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				held := now.Sub(started)
				waited := s.writeMu.waiting.Load() > 0 || s.writeDB.Stats().WaitCount > poolWaits
				if (waited && held >= s.maintenanceYieldAfter) || held >= s.maintenanceHoldLimit {
					cancel(ports.ErrMaintenanceYielded)
					return
				}
			}
		}
	}()
	n, err := fn(runCtx)
	close(stop)
	if err != nil && errors.Is(context.Cause(runCtx), ports.ErrMaintenanceYielded) {
		s.maintenanceYields.Add(1)
		return 0, ports.ErrMaintenanceYielded
	}
	return n, err
}

// MaintenanceYields is how many background writes have given the writer back
// to a foreground write since the store opened.
func (s *Store) MaintenanceYields() int64 { return s.maintenanceYields.Load() }
