//go:build windows

package fslink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// linkCount is the file's number of names (NumberOfLinks), read the way
// `fsutil hardlink list` does: a hard link raises it, a copy does not.
func linkCount(t *testing.T, path string) uint32 {
	t.Helper()
	name, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &data); err != nil {
		t.Fatalf("file information %s: %v", path, err)
	}
	return data.NumberOfLinks
}

// node_modules paths run past MAX_PATH; the farm's own Windows API calls must
// reach them.
func TestHardLinkFarmLongPaths(t *testing.T) {
	deps := filepath.Join(t.TempDir(), "project", "node_modules")
	deep := deps
	for len(deep) < 300 {
		deep = filepath.Join(deep, "node_modules", strings.Repeat("p", 20))
	}
	file := filepath.Join(deep, "index.js")
	writeTree(t, file, "deep\n")
	alias := filepath.Join(deps, "alias")
	linkDirOrSkip(t, deep, alias)
	target := filepath.Join(t.TempDir(), "node_modules")
	stats, err := HardLinkFarm(deps, target, FarmOptions{})
	if err != nil {
		t.Fatalf("HardLinkFarm: %v", err)
	}
	if stats.Files != 1 || stats.Links != 1 {
		t.Fatalf("stats = %+v, want 1 file and 1 link", stats)
	}
	rel, _ := filepath.Rel(deps, file)
	if n := linkCount(t, filepath.Join(target, rel)); n != 2 {
		t.Fatalf("deep farm file has %d names, want 2", n)
	}
	if data, err := os.ReadFile(filepath.Join(target, "alias", "index.js")); err != nil || string(data) != "deep\n" {
		t.Fatalf("deep junction in the farm = %q, %v", data, err)
	}
}
