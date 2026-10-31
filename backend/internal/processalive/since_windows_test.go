//go:build windows

package processalive

import (
	"os"
	"testing"
	"time"
)

func TestAliveSinceRejectsAProcessStartedAfterTheOwner(t *testing.T) {
	pid := os.Getpid()
	if !AliveSince(pid, time.Now()) {
		t.Fatal("this process started before now and is alive")
	}
	if !AliveSince(pid, time.Time{}) {
		t.Fatal("a zero start time falls back to Alive")
	}
	// A descriptor written before this process existed names an owner this
	// process cannot be: the id was reused.
	if AliveSince(pid, time.Now().Add(-24*time.Hour)) {
		t.Fatal("a process started after the recorded start is another program")
	}
	if AliveSince(0, time.Now()) {
		t.Fatal("pid 0 is never alive")
	}
}
