package piacp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A daemon from before Pi's approvals is stopped on its control socket, as
// `pi-acp --daemon-stop` does, before AO starts one with the approval env.
func TestStopDaemonShutsTheDaemonDownOnItsControlSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "c.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	var shutdowns atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/shutdown" {
			shutdowns.Add(1)
			w.WriteHeader(http.StatusOK)
			go func() { _ = listener.Close() }()
			return
		}
		http.NotFound(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	ready := func() bool {
		conn, err := net.DialTimeout("unix", socket, 250*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := stopDaemon(ctx, socket, ready); err != nil {
		t.Fatalf("stopDaemon: %v", err)
	}
	if shutdowns.Load() != 1 || ready() {
		t.Fatalf("shutdowns = %d, still listening = %v", shutdowns.Load(), ready())
	}
}

func TestWaitForDaemonHandlesLateReadinessAndCancellation(t *testing.T) {
	t.Run("waits", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		probes := 0
		if err := waitForDaemon(ctx, func() bool {
			probes++
			return probes == 3
		}, nil); err != nil || probes != 3 {
			t.Fatalf("waitForDaemon: probes=%d error=%v", probes, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		if err := waitForDaemon(ctx, func() bool { cancel(); return false }, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("waitForDaemon: %v, want context.Canceled", err)
		}
	})
	t.Run("child exit", func(t *testing.T) {
		exited := make(chan error, 1)
		exited <- nil
		if err := waitForDaemon(context.Background(), func() bool { return false }, exited); err == nil {
			t.Fatal("waitForDaemon accepted an exited child without a listening socket")
		}
	})
}
