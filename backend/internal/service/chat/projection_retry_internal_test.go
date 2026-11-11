package chat

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type sqliteCodeError struct{ code int }

func (e sqliteCodeError) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e sqliteCodeError) Code() int     { return e.code }

// A busy store used to cost a chat its three tries in 60 ms and detach it from
// its running agent. It is now waited for, for about a minute.
func TestProjectionRetryWaitsOutABusyStoreButNotARealFault(t *testing.T) {
	busy := []error{
		sqliteCodeError{5},   // SQLITE_BUSY
		sqliteCodeError{6},   // SQLITE_LOCKED
		sqliteCodeError{261}, // SQLITE_BUSY_RECOVERY
		fmt.Errorf("upsert activity: %w", sqliteCodeError{517}), // SQLITE_BUSY_SNAPSHOT, wrapped
		fmt.Errorf("project: %w", context.DeadlineExceeded),
	}
	for _, err := range busy {
		var total time.Duration
		tries := 0
		for {
			wait, again := projectionRetryWait(err, tries)
			if !again {
				break
			}
			total += wait
			tries++
		}
		if tries != len(projectionBusyWaits) || tries < 8 {
			t.Fatalf("%v: %d tries, want %d", err, tries, len(projectionBusyWaits))
		}
		if total < 20*time.Second || total > 2*time.Minute {
			t.Fatalf("%v: waits add up to %s, want about half a minute before SQLite's own busy timeouts", err, total)
		}
	}
	for _, err := range []error{errors.New("no such table"), sqliteCodeError{19}, context.Canceled} {
		tries := 0
		for {
			wait, again := projectionRetryWait(err, tries)
			if !again {
				break
			}
			if wait != 20*time.Millisecond {
				t.Fatalf("%v: wait %s, want the quick 20 ms", err, wait)
			}
			tries++
		}
		if tries != 3 {
			t.Fatalf("%v: %d tries, want the three quick ones", err, tries)
		}
	}
}
