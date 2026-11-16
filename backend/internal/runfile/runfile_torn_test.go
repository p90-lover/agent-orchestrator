package runfile

import (
	"os"
	"path/filepath"
	"testing"
)

// A power loss can leave running.json empty, cut short or filled with zero
// bytes: the rename reached the disk and the content did not. CheckStale
// returned the parse error, so the daemon refused every start ("inspect
// run-file") until the file was deleted by hand.
func TestCheckStaleTreatsTornRunFileAsStale(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":      {},
		"cut short":  []byte(`{"pid": 4242, "port": 30`),
		"zero bytes": make([]byte, 64),
		"not json":   []byte("running"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "running.json")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			live, err := CheckStale(path)
			if err != nil {
				t.Fatalf("CheckStale = %v, want a torn run-file reported as stale", err)
			}
			if live != nil {
				t.Fatalf("CheckStale = %+v, want no live daemon for a torn run-file", live)
			}
			// The start that follows overwrites it.
			if err := Write(path, Info{PID: os.Getpid(), Port: 3001}); err != nil {
				t.Fatalf("Write over a torn run-file: %v", err)
			}
			got, err := Read(path)
			if err != nil || got == nil || got.Port != 3001 {
				t.Fatalf("Read after overwrite = %+v, %v", got, err)
			}
		})
	}
}

// A run-file that cannot be read at all says nothing about a live daemon, so it
// still stops the start.
func TestCheckStaleStillFailsOnUnreadableRunFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "running.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckStale(path); err == nil {
		t.Fatal("CheckStale accepted a run-file path it could not read")
	}
}
