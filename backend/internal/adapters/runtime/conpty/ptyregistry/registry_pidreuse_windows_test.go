//go:build windows

package ptyregistry

import (
	"context"
	"os"
	"testing"
	"time"
)

// An entry whose process id now belongs to a program started after the entry
// was registered is a dead host's entry. It used to stay for as long as that
// other program ran. The entries here use this test process's own id.
func TestScanPrunesEntryWhoseProcessIDWasReused(t *testing.T) {
	setupHome(t)
	ctx := context.Background()
	pid := os.Getpid()
	for _, e := range []Entry{
		{SessionID: "reused", PtyHostPID: pid, PipePath: "127.0.0.1:50001",
			RegisteredAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)},
		{SessionID: "current", PtyHostPID: pid, PipePath: "127.0.0.1:50002", RegisteredAt: nowRFC3339()},
		{SessionID: "undated", PtyHostPID: pid, PipePath: "127.0.0.1:50003"},
	} {
		if err := Register(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	entries, complete, err := Scan(ctx)
	if err != nil || !complete {
		t.Fatalf("Scan: complete=%v err=%v", complete, err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.SessionID] = true
	}
	if got["reused"] {
		t.Fatal("an entry whose process id went to a later program was kept as a live host")
	}
	if !got["current"] || !got["undated"] {
		t.Fatalf("entries = %+v, want the current and the undated entry kept", entries)
	}
}
