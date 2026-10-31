//go:build windows

package persistenthost

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

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
