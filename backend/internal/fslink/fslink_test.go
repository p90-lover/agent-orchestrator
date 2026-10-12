package fslink

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// linkDirOrSkip links a directory: a symlink on Unix, a junction on Windows,
// which needs no privilege and so must always succeed there.
func linkDirOrSkip(t *testing.T, source, target string) Method {
	t.Helper()
	method, err := Link(source, target)
	if err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("Link(%q): %v", source, err)
	}
	return method
}

// sharedDeps lays out a project dependency folder holding one file and returns
// the folder and the file.
func sharedDeps(t *testing.T) (string, string) {
	t.Helper()
	deps := filepath.Join(t.TempDir(), "project", "node_modules")
	if err := os.MkdirAll(filepath.Join(deps, "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(deps, "pkg", "index.js")
	if err := os.WriteFile(file, []byte("module.exports = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return deps, file
}

func TestLinkDirectory(t *testing.T) {
	deps, _ := sharedDeps(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "node_modules")
	method := linkDirOrSkip(t, deps, target)
	if runtime.GOOS == "windows" && method != MethodJunction {
		t.Fatalf("directory link method = %q, want junction", method)
	}
	if isLink, err := IsLink(target); err != nil || !isLink {
		t.Fatalf("IsLink(%q) = %v, %v; want true", target, isLink, err)
	}
	data, err := os.ReadFile(filepath.Join(target, "pkg", "index.js"))
	if err != nil || string(data) != "module.exports = 1\n" {
		t.Fatalf("read through link = %q, %v", data, err)
	}
	resolved, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if !sameFile(t, resolved, deps) {
		t.Fatalf("link target = %q, want %q", resolved, deps)
	}
}

func TestLinkFile(t *testing.T) {
	project := t.TempDir()
	source := filepath.Join(project, ".env")
	if err := os.WriteFile(source, []byte("X=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), ".env")
	method, err := Link(source, target)
	if err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("Link file: %v", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "X=1" {
		t.Fatalf("linked file = %q, %v (method %s)", data, err, method)
	}
}

func TestLinkMissingSource(t *testing.T) {
	if _, err := Link(filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Link(missing) error = %v, want not exist", err)
	}
}

// TestRemoveAllDoesNotFollowLinks pins the property teardown relies on:
// os.RemoveAll of a workspace unlinks a linked dependency folder and leaves the
// project's real folder, and every file in it, in place.
func TestRemoveAllDoesNotFollowLinks(t *testing.T) {
	deps, file := sharedDeps(t)
	workspace := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(filepath.Join(workspace, "frontend"), 0o750); err != nil {
		t.Fatal(err)
	}
	linkDirOrSkip(t, deps, filepath.Join(workspace, "frontend", "node_modules"))
	linkDirOrSkip(t, deps, filepath.Join(workspace, "node_modules"))

	if err := os.RemoveAll(workspace); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Lstat(workspace); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("workspace still present: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "module.exports = 1\n" {
		t.Fatalf("project dependency file after RemoveAll = %q, %v; want untouched", data, err)
	}
}

func TestUnlinkExternalAndRestore(t *testing.T) {
	deps, file := sharedDeps(t)
	workspace := filepath.Join(t.TempDir(), "worktree")
	inner := filepath.Join(workspace, "lib")
	if err := os.MkdirAll(filepath.Join(workspace, "frontend"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inner, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "own.txt"), []byte("own"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(workspace, "frontend", "node_modules")
	internal := filepath.Join(workspace, "lib-alias")
	linkDirOrSkip(t, deps, external)
	linkDirOrSkip(t, inner, internal)

	removed, err := UnlinkExternal(workspace)
	if err != nil {
		t.Fatalf("UnlinkExternal: %v", err)
	}
	if len(removed) != 1 || removed[0].Path != external || !sameFile(t, removed[0].Target, deps) {
		t.Fatalf("removed = %+v, want only %q", removed, external)
	}
	if _, err := os.Lstat(external); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("external link still present: %v", err)
	}
	if isLink, err := IsLink(internal); err != nil || !isLink {
		t.Fatalf("link inside the workspace was removed: %v, %v", isLink, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("project dependency file after unlink: %v", err)
	}

	if err := Restore(removed); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "pkg", "index.js")); err != nil {
		t.Fatalf("restored link does not reach the project folder: %v", err)
	}
}

func TestUnlinkExternalMissingRoot(t *testing.T) {
	removed, err := UnlinkExternal(filepath.Join(t.TempDir(), "gone"))
	if err != nil || len(removed) != 0 {
		t.Fatalf("UnlinkExternal(missing) = %v, %v; want nothing", removed, err)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
