package sessionmanager

import (
	"context"
	"testing"
	"time"
)

// A startup restore waits while a user is starting a chat, and goes on as soon
// as that spawn is done.
func TestStartupRestoreYieldsToUserSpawns(t *testing.T) {
	m := &Manager{}
	start := time.Now()
	m.waitForUserSpawns(context.Background(), time.Second)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited although no spawn was in flight")
	}

	m.userSpawns.Add(1)
	done := make(chan struct{})
	go func() { m.waitForUserSpawns(context.Background(), time.Minute); close(done) }()
	select {
	case <-done:
		t.Fatal("the restore went ahead while a user spawn was in flight")
	case <-time.After(400 * time.Millisecond):
	}
	m.userSpawns.Add(-1)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the restore did not resume after the spawn finished")
	}

	// A spawn that never ends cannot hold a restore past its limit.
	m.userSpawns.Add(1)
	start = time.Now()
	m.waitForUserSpawns(context.Background(), 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("waited %v; want about the 300ms limit", elapsed)
	}
}
