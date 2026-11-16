//go:build windows

package conpty

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The session is already in memory: the host died while the daemon ran and its
// id went to another program. Its old address is not dialed either, because a
// kill request stops whatever pty-host listens there.
func TestDestroyOfKnownSessionWhoseHostIDWasReusedTouchesNothing(t *testing.T) {
	isolateRegistry(t)
	rt := New(Options{})
	rt.sessions["sess-known"] = &hostSession{
		addr: closedLoopbackAddr(t), pid: os.Getpid(), launchID: "launch-old",
		registeredAt: time.Now().Add(-24 * time.Hour),
	}
	rt.destroyWait = 50 * time.Millisecond
	dials, kills := 0, 0
	rt.killHost = func(string) error {
		dials++
		return nil
	}
	rt.processFinder = func(int) (processKiller, error) {
		return processKillerFunc(func() error {
			kills++
			return nil
		}), nil
	}

	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-known"}); err != nil {
		t.Fatalf("Destroy = %v, want the session torn down: its host is gone", err)
	}
	if dials != 0 || kills != 0 {
		t.Fatalf("Destroy sent %d kill request(s) and %d force kill(s), want none", dials, kills)
	}
}

// A host registered after this process started is this process as far as the
// start time can tell, so the check must not get in the way of a real teardown.
func TestDestroyStillKillsAHostRegisteredAfterItStarted(t *testing.T) {
	isolateRegistry(t)
	rt := New(Options{})
	rt.sessions["sess-live"] = &hostSession{
		addr: closedLoopbackAddr(t), pid: os.Getpid(), launchID: "launch-live", registeredAt: time.Now(),
	}
	rt.destroyWait = 50 * time.Millisecond
	rt.killHost = func(string) error { return nil }
	kills := 0
	rt.processFinder = func(int) (processKiller, error) {
		return processKillerFunc(func() error {
			kills++
			return nil
		}), nil
	}

	if err := rt.Destroy(context.Background(), ports.RuntimeHandle{ID: "sess-live"}); err == nil {
		t.Fatal("Destroy reported success although its host process is still running")
	}
	if kills != 1 {
		t.Fatalf("force kills = %d, want 1 for a host that ignored the kill request", kills)
	}
}
