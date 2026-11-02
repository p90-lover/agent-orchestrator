package persistenthost

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func withUnattachedHostTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := unattachedHostTimeout
	unattachedHostTimeout = timeout
	t.Cleanup(func() { unattachedHostTimeout = previous })
}

// A host whose spawning daemon gave up before it published its descriptor is
// never attached; it must not keep its provider running forever.
func TestHostWithoutControllerShutsItselfDown(t *testing.T) {
	withUnattachedHostTimeout(t, 200*time.Millisecond)
	cfg := Config{
		SessionID: "never-attached", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
	hostDone := make(chan error, 1)
	go func() { hostDone <- Run(context.Background(), cfg) }()
	awaitDescriptor(t, cfg.DataDir, cfg.SessionID)

	select {
	case err := <-hostDone:
		if err != nil {
			t.Fatalf("host exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host with no controller kept running")
	}
	if _, err := readDescriptor(cfg.DataDir, cfg.SessionID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor after self-shutdown: %v", err)
	}
}

// Once a controller has attached, a detached host waits for the daemon to come
// back: surviving a daemon restart is what the host is for.
func TestHostThatHadControllerWaitsForReattach(t *testing.T) {
	withUnattachedHostTimeout(t, 200*time.Millisecond)
	cfg := Config{
		SessionID: "was-attached", DataDir: t.TempDir(), Workdir: t.TempDir(),
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
	hostDone := make(chan error, 1)
	go func() { hostDone <- Run(context.Background(), cfg) }()
	d := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
	first, err := attach(context.Background(), d, false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	pid := requestProviderPID(t, first, 1, "pid")
	_ = first.Stdin.Close()

	select {
	case err := <-hostDone:
		t.Fatalf("detached host exited past the unattached timeout: %v", err)
	case <-time.After(600 * time.Millisecond):
	}
	var second *Transport
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if second, err = attach(context.Background(), d, true); err == nil {
			break
		}
		if !errors.Is(err, ErrAttached) {
			t.Fatalf("reattach: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("host never released the first controller")
	}
	if got := requestProviderPID(t, second, 2, "pid-again"); got != pid {
		t.Fatalf("provider pid changed across detach: %d -> %d", pid, got)
	}
	_ = second.Stdin.Close()
	if err := Shutdown(context.Background(), cfg.DataDir, cfg.SessionID); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-hostDone:
	case <-time.After(5 * time.Second):
		t.Fatal("host did not exit after explicit shutdown")
	}
}
