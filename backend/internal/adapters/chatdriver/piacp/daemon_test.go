package piacp

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
