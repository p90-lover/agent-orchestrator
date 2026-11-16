//go:build windows

package conpty

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/conpty/ptyregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// closedLoopbackAddr returns a loopback address nothing listens on.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	return addr
}

// A pty-host that died leaves its process id in the registry, and Windows hands
// that id to the next program: after a reboot, often an unrelated one. The entry
// then looked alive, so Destroy waited on that program and force-killed it.
//
// The entry here was registered a day ago under this test process's own id, so
// the process holding the id started after the registration and cannot be the
// host. The kill is stubbed: a real one would end the test run.
func TestDestroyLeavesAProcessThatReusedTheHostIDAlone(t *testing.T) {
	isolateRegistry(t)
	ctx := context.Background()
	if err := ptyregistry.Register(ctx, ptyregistry.Entry{
		SessionID: "sess-reused", PtyHostPID: os.Getpid(), PipePath: closedLoopbackAddr(t),
		LaunchID:     "launch-old",
		RegisteredAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	rt := New(Options{})
	rt.destroyWait = 50 * time.Millisecond
	kills := 0
	rt.processFinder = func(int) (processKiller, error) {
		return processKillerFunc(func() error {
			kills++
			return nil
		}), nil
	}

	err := rt.Destroy(ctx, ports.RuntimeHandle{ID: "sess-reused"})
	if kills != 0 {
		t.Fatalf("Destroy tried %d time(s) to kill pid %d, which is not the pty-host (err %v)", kills, os.Getpid(), err)
	}
	if err != nil {
		t.Fatalf("Destroy = %v, want the session torn down: its host is gone", err)
	}
	entries, complete, scanErr := ptyregistry.Scan(ctx)
	if scanErr != nil || !complete || len(entries) != 0 {
		t.Fatalf("registry after Destroy = %+v complete=%v err=%v, want it empty", entries, complete, scanErr)
	}
}
