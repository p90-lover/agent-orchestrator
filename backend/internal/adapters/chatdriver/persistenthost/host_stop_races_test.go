package persistenthost

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// failDescriptorReads makes the numbered reads of host.json (1 is the next one)
// fail the way an open fails on Windows while another process deletes the file.
func failDescriptorReads(t *testing.T, failing ...int64) {
	t.Helper()
	previous := readDescriptorFile
	var calls atomic.Int64
	readDescriptorFile = func(name string) ([]byte, error) {
		call := calls.Add(1)
		for _, n := range failing {
			if call == n {
				return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("the file is being used by another process")}
			}
		}
		return previous(name)
	}
	t.Cleanup(func() { readDescriptorFile = previous })
}

// The daemon reads host.json while the host it is stopping deletes it. On
// Windows a read that collides with the delete fails to open the file. Shutdown
// returned that error, so a host that was exiting was reported as one that
// could not be stopped (seen 2026-11-16 in the replacement of a host with no
// session).
func TestShutdownWaitsOutDescriptorItCannotReadJustNow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing []int64
	}{
		{"at its first read", []int64{1, 2}},
		{"while the host exits", []int64{2, 3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rawHostConfig(t, "busy-descriptor")
			hostDone := make(chan error, 1)
			go func() { hostDone <- Run(context.Background(), cfg) }()
			awaitDescriptor(t, cfg.DataDir, cfg.SessionID)

			failDescriptorReads(t, tc.failing...)
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
		})
	}
}

// The host must get its own files deleted although a reader has one open when
// it first tries. Only Windows refuses that delete; elsewhere this passes
// trivially.
func TestRemoveOwnedFileWaitsOutAReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(path) //nolint:gosec // test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = reader.Close()
		close(released)
	}()

	removeOwnedFile(path)
	<-released
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file after removeOwnedFile: %v, want it gone", err)
	}
}
