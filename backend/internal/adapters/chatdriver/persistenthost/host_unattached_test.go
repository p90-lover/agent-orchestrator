package persistenthost

import (
	"bufio"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func acpHostConfig(t *testing.T, sessionID string) Config {
	t.Helper()
	cfg := Config{
		SessionID: sessionID, DataDir: t.TempDir(), Workdir: t.TempDir(), Protocol: ProtocolACP,
		Env:  append(os.Environ(), "AO_CHAT_HOST_PROVIDER_HELPER=1", "AO_CHAT_HOST_ACP_HELPER=1"),
		Argv: []string{os.Args[0], "-test.run=TestProviderHelper"},
	}
	// A failing run must not leave the host and its provider behind.
	t.Cleanup(func() { _ = Shutdown(context.Background(), cfg.DataDir, cfg.SessionID) })
	return cfg
}

// A daemon that dies after attaching to a new ACP host, but before a session
// is open on it, leaves a host with nothing to resume. Having had a controller
// kept it running for good; it must stop itself like a host nobody attached to.
func TestACPHostThatNeverOpenedASessionShutsItselfDown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		initialize bool
	}{
		{"attached only", false},
		{"initialized only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnattachedHostTimeout(t, 200*time.Millisecond)
			cfg := acpHostConfig(t, "no-session")
			hostDone := make(chan error, 1)
			go func() { hostDone <- Run(context.Background(), cfg) }()
			d := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
			controller, err := attach(context.Background(), d, false)
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if tc.initialize {
				sendFrame(t, controller, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
				_ = readFrame(t, bufio.NewReader(controller.Stdout))
			}
			// Stay attached past the timeout: a host whose controller is still
			// setting it up is not abandoned.
			select {
			case err := <-hostDone:
				t.Fatalf("host exited while its controller was attached: %v", err)
			case <-time.After(500 * time.Millisecond):
			}
			_ = controller.Stdin.Close()

			select {
			case err := <-hostDone:
				if err != nil {
					t.Fatalf("host exit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ACP host with no session kept running after its controller left")
			}
			if _, err := readDescriptor(cfg.DataDir, cfg.SessionID); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("descriptor after self-shutdown: %v", err)
			}
		})
	}
}

// An ACP host that holds a session is what a daemon restart must find again.
func TestACPHostWithASessionWaitsForReattach(t *testing.T) {
	withUnattachedHostTimeout(t, 200*time.Millisecond)
	cfg := acpHostConfig(t, "has-session")
	hostDone := make(chan error, 1)
	go func() { hostDone <- Run(context.Background(), cfg) }()
	d := awaitDescriptor(t, cfg.DataDir, cfg.SessionID)
	first, err := attach(context.Background(), d, false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	reader := bufio.NewReader(first.Stdout)
	sendFrame(t, first, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	_ = readFrame(t, reader)
	sendFrame(t, first, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`)
	_ = readFrame(t, reader)
	_ = first.Stdin.Close()

	select {
	case err := <-hostDone:
		t.Fatalf("detached host with a session exited past the unattached timeout: %v", err)
	case <-time.After(600 * time.Millisecond):
	}
	second := awaitAttach(t, d)
	if second.ACPState == nil || second.ACPState.SessionID != "acp-session-live" {
		t.Fatalf("reconnect state = %+v, want the session opened before the detach", second.ACPState)
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
