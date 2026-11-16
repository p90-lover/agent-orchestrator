//go:build windows

package persistenthost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// A host that dies before it publishes host.json leaves only host.lock, which
// names its process id. Once Windows gives that id to another program the lock
// read as held for as long as that program ran, and no host could start for
// the chat. The descriptor already compares start times; the lock did not.
func TestAcquireHostLockReclaimsALockWhoseProcessIDWasReused(t *testing.T) {
	dataDir := t.TempDir()
	write := func(sessionID string, writtenAt time.Time) {
		t.Helper()
		dir, err := hostDir(dataDir, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path, _ := lockPath(dataDir, sessionID)
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, writtenAt, writtenAt); err != nil {
			t.Fatal(err)
		}
	}
	// This test process started after the lock was written: its id was reused.
	write("reused", time.Now().Add(-24*time.Hour))
	release, err := acquireHostLock(dataDir, "reused")
	if err != nil {
		t.Fatalf("a lock whose owner's id another process took is abandoned: %v", err)
	}
	release()
	// A lock its live owner wrote just now still fences a second host.
	write("held", time.Now())
	if _, err := acquireHostLock(dataDir, "held"); !errors.Is(err, ErrHostExists) {
		t.Fatalf("a live owner's lock: err = %v, want ErrHostExists", err)
	}
}

// Seen 2026-10-31: a chat host died during an app update and Windows gave its
// process id to an unrelated bun.exe. AO kept reading the host as alive, so
// every exit-agent failed on the dead host's closed port, for hours.
func TestShutdownTreatsAReusedProcessIDAsADeadHost(t *testing.T) {
	dataDir := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	write := func(sessionID string, startedAt time.Time) {
		t.Helper()
		if err := writeDescriptor(dataDir, Descriptor{
			Version: ProtocolVersion, SessionID: sessionID, Address: address, PID: os.Getpid(), Token: "test", StartedAt: startedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// This test process started after the recorded host did: its id was reused.
	write("reused", time.Now().Add(-24*time.Hour))
	if err := Shutdown(context.Background(), dataDir, "reused"); err != nil {
		t.Fatalf("a host whose id another process took is dead: %v", err)
	}
	// A live owner that does not answer still fails closed.
	write("silent", time.Now().UTC())
	if err := Shutdown(context.Background(), dataDir, "silent"); err == nil {
		t.Fatal("a live owner whose socket refused must not count as stopped")
	}
}
