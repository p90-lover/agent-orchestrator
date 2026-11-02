//go:build windows

package fslink

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// withoutSymlinkPrivilege makes file symlinks fail the way they do on a host
// without Developer Mode or elevation.
func withoutSymlinkPrivilege(t *testing.T) {
	t.Helper()
	previous := symlinkFile
	symlinkFile = func(string, string) error {
		return &os.LinkError{Op: "symlink", Err: windows.ERROR_PRIVILEGE_NOT_HELD}
	}
	t.Cleanup(func() { symlinkFile = previous })
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinkFileFallsBackToHardLink(t *testing.T) {
	withoutSymlinkPrivilege(t)
	source := filepath.Join(t.TempDir(), ".env")
	writeFile(t, source, 3)
	target := filepath.Join(t.TempDir(), ".env")
	method, err := Link(source, target)
	if err != nil || method != MethodHardlink {
		t.Fatalf("Link = %q, %v; want hardlink", method, err)
	}
	if !sameFile(t, source, target) {
		t.Fatal("hard link does not share the source file")
	}
}

func TestLinkFileFallsBackToCopy(t *testing.T) {
	withoutSymlinkPrivilege(t)
	previous := hardLink
	hardLink = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { hardLink = previous })

	source := filepath.Join(t.TempDir(), ".env")
	writeFile(t, source, 3)
	target := filepath.Join(t.TempDir(), ".env")
	if method, err := Link(source, target); err != nil || method != MethodCopy {
		t.Fatalf("Link = %q, %v; want copy", method, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "xxx" {
		t.Fatalf("copied file = %q, %v", data, err)
	}

	// A large file is refused rather than duplicated.
	big := filepath.Join(t.TempDir(), "big.bin")
	writeFile(t, big, MaxCopyBytes+1)
	if _, err := Link(big, filepath.Join(t.TempDir(), "big.bin")); err == nil {
		t.Fatal("Link copied a file over MaxCopyBytes")
	}
}

// The junction itself must be a reparse point, not a copy of the folder.
func TestJunctionIsReparsePoint(t *testing.T) {
	deps, _ := sharedDeps(t)
	target := filepath.Join(t.TempDir(), "node_modules")
	if method, err := Link(deps, target); err != nil || method != MethodJunction {
		t.Fatalf("Link = %q, %v; want junction", method, err)
	}
	name, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatalf("attributes %#x have no reparse point", attrs)
	}
}
