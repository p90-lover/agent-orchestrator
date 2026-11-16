package persistenthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rawHostConfig(t *testing.T, sessionID string) Config {
	t.Helper()
	return Config{
		SessionID: sessionID, DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
}

// awaitListenerClosed returns once nothing accepts connections at address, or
// after 15 s. It is safe to call from a goroutine other than the test's.
func awaitListenerClosed(address string) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
}

// The daemon reads host.json every 20 ms while it waits for a host to exit, and
// the host deletes that file as its last step. On Windows a delete fails while
// another handle has the file open. The host tried once, so a delete that met a
// read left host.json behind for good: the daemon kept seeing the old
// descriptor, ran out its whole wait and reported "wait for chat host shutdown:
// context deadline exceeded" for a host that had stopped.
//
// The reader here is held from before the host's first attempt until shortly
// after it. Elsewhere than Windows nothing refuses the delete and this passes
// trivially.
func TestHostDeletesDescriptorAlthoughAReaderHadItOpen(t *testing.T) {
	cfg := rawHostConfig(t, "read-while-exiting")
	hostDone := make(chan error, 1)
	go func() { hostDone <- Run(context.Background(), cfg) }()
	d := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
	path, err := descriptorPath(cfg.DataDir, cfg.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path) //nolint:gosec // test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// The host closes its listener just before it deletes its files.
		awaitListenerClosed(d.Address)
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
	}()

	if err := Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-hostDone:
		if err != nil {
			t.Fatalf("host exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host did not exit after shutdown")
	}
	if _, err := readDescriptor(cfg.DataDir, cfg.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor after the host exited: %v, want it deleted", err)
	}
}

// A host that is killed deletes nothing, so host.json can outlive it. Shutdown
// kept seeing the old descriptor and ran out its whole wait, although the
// owner's process was gone. Before the shutdown request a dead owner was already
// accepted; this is the same answer after it.
func TestShutdownAcceptsDeadOwnerWhoseDescriptorOutlivedIt(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, readErr := bufio.NewReader(conn).ReadBytes('\n'); readErr != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(helloResponse{OK: true})
		// Hold the connection the way an exiting host does until it is gone.
		time.Sleep(3 * time.Second)
	}()
	dataDir := t.TempDir()
	const sessionID = "outlived"
	if err := writeDescriptor(dataDir, Descriptor{
		Version: ProtocolVersion, SessionID: sessionID, Address: listener.Addr().String(),
		Token: "token", PID: 2147483647,
	}); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() { result <- Shutdown(context.Background(), dataDir, sessionID) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown kept waiting for a host whose process is gone")
	}
}

// A descriptor that is present but not a valid one is still an unknown owner:
// waiting out a file that is busy must not turn that into a wait or a pass.
func TestShutdownRejectsMalformedDescriptorAtOnce(t *testing.T) {
	dataDir := t.TempDir()
	const sessionID = "malformed"
	path, err := descriptorPath(dataDir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := Shutdown(context.Background(), dataDir, sessionID); err == nil {
		t.Fatal("shutdown accepted a malformed descriptor")
	}
	if waited := time.Since(started); waited > 100*time.Millisecond {
		t.Fatalf("shutdown waited %s on a malformed descriptor, want it reported at once", waited)
	}
}
