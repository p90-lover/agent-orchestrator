//go:build !windows

package fslink

import (
	"os"
	"syscall"
	"testing"
)

// linkCount is the file's number of names (st_nlink): a hard link raises it,
// a copy does not.
func linkCount(t *testing.T, path string) uint32 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no link count on this platform")
	}
	return uint32(stat.Nlink) //nolint:gosec,unconvert // a test file has a handful of names
}
