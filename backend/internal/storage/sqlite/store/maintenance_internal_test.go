package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// slowDelete is a delete SQLite cannot finish in the life of a test: its filter
// counts a recursive series first. Only an interrupt ends it.
const slowDelete = `DELETE FROM change_log WHERE (
  WITH RECURSIVE series(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM series WHERE x < 4000000000)
  SELECT COUNT(*) FROM series) >= 0`

func maintenanceFixture(t *testing.T, yieldAfter, holdLimit time.Duration) *Store {
	t.Helper()
	s := retentionReaderFixture(t)
	s.maintenanceYieldAfter, s.maintenanceHoldLimit = yieldAfter, holdLimit
	if _, err := s.writeDB.Exec(`INSERT INTO change_log(seq, created_at) VALUES (1, '2026-01-01'), (2, '2026-01-02'), (3, '2026-01-03')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func slowMaintenance(s *Store) (int64, error) {
	return s.maintain(context.Background(), func(ctx context.Context) (int64, error) {
		result, err := s.writeDB.ExecContext(ctx, slowDelete)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
}

func changeLogRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The live failure (2026-11-11): cleanup held the writer for minutes while chat
// writes waited. A waiting write now gets the writer within the yield time.
func TestMaintenanceYieldsTheWriterToAWaitingWrite(t *testing.T) {
	for _, waiter := range []string{"on the writer lock", "on the writer connection"} {
		t.Run(waiter, func(t *testing.T) {
			s := maintenanceFixture(t, 100*time.Millisecond, time.Hour)
			result := make(chan error, 1)
			go func() { _, err := slowMaintenance(s); result <- err }()
			deadline := time.Now().Add(5 * time.Second)
			for s.writeMu.heldFor(time.Now()) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("maintenance never took the writer")
				}
				time.Sleep(time.Millisecond)
			}
			started := time.Now()
			if waiter == "on the writer lock" {
				s.writeMu.Lock()
				defer s.writeMu.Unlock()
			}
			// Most writes run on the one writer connection without the lock.
			if _, err := s.writeDB.Exec(`INSERT INTO change_log(seq, created_at) VALUES (9, '2026-02-01')`); err != nil {
				t.Fatalf("the waiting write failed: %v", err)
			}
			if waited := time.Since(started); waited > 3*time.Second {
				t.Fatalf("the write waited %s behind maintenance", waited)
			}
			if err := <-result; !errors.Is(err, ports.ErrMaintenanceYielded) {
				t.Fatalf("maintenance returned %v, want ErrMaintenanceYielded", err)
			}
			if got := changeLogRows(t, s); got != 4 {
				t.Fatalf("change_log has %d rows, want the 3 kept (the interrupted delete rolled back) plus the write", got)
			}
			if got := s.MaintenanceYields(); got != 1 {
				t.Fatalf("MaintenanceYields = %d, want 1", got)
			}
		})
	}
}

func TestMaintenanceWithNoWaiterStopsAtItsHoldLimit(t *testing.T) {
	s := maintenanceFixture(t, time.Hour, 150*time.Millisecond)
	started := time.Now()
	if _, err := slowMaintenance(s); !errors.Is(err, ports.ErrMaintenanceYielded) {
		t.Fatalf("maintenance returned %v, want ErrMaintenanceYielded", err)
	}
	if held := time.Since(started); held > 3*time.Second {
		t.Fatalf("maintenance held the writer %s past its limit", held)
	}
	if s.writeMu.heldFor(time.Now()) != 0 {
		t.Fatal("the writer lock was not released")
	}
	if got := changeLogRows(t, s); got != 3 {
		t.Fatalf("change_log has %d rows, want 3", got)
	}
}

func TestMaintenanceThatFinishesInTimeIsNotInterrupted(t *testing.T) {
	s := maintenanceFixture(t, 50*time.Millisecond, time.Hour)
	n, err := s.maintain(context.Background(), func(ctx context.Context) (int64, error) {
		result, err := s.writeDB.ExecContext(ctx, `DELETE FROM change_log WHERE seq <= 2`)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	})
	if err != nil || n != 2 {
		t.Fatalf("maintain = %d, %v; want 2, nil", n, err)
	}
	if s.MaintenanceYields() != 0 {
		t.Fatal("a finished maintenance write counted as a yield")
	}
}

// A caller that gives up is not a yield: the janitor must not shrink its batch for it.
func TestMaintenanceCallerCancelIsNotAYield(t *testing.T) {
	s := maintenanceFixture(t, time.Hour, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := s.maintain(ctx, func(ctx context.Context) (int64, error) {
		_, err := s.writeDB.ExecContext(ctx, slowDelete)
		return 0, err
	})
	if err == nil || errors.Is(err, ports.ErrMaintenanceYielded) {
		t.Fatalf("maintain returned %v, want the caller's cancellation", err)
	}
	if s.MaintenanceYields() != 0 {
		t.Fatal("a cancelled caller counted as a yield")
	}
}

// Retention goes through maintain: a real prune yields too.
func TestChangeLogRetentionDeleteYields(t *testing.T) {
	s := maintenanceFixture(t, time.Hour, time.Nanosecond)
	if _, err := s.PruneChangeLogToMaxRows(context.Background(), 0, 10); err != nil && !errors.Is(err, ports.ErrMaintenanceYielded) {
		t.Fatalf("prune returned %v", err)
	}
	// With a hold limit this short the delete either finished first or yielded;
	// both leave the table whole or pruned, never half-written.
	if got := changeLogRows(t, s); got != 0 && got != 3 {
		t.Fatalf("change_log has %d rows, want 0 or 3", got)
	}
}
