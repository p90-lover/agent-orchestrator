package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rewriteKeepingStat replaces a file's bytes with others of the same length and
// puts its modification time back: to the cache it is the same file.
func rewriteKeepingStat(t *testing.T, path string, content string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(content)) != info.Size() {
		t.Fatalf("content must keep the size: %d != %d", len(content), info.Size())
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func touchLater(t *testing.T, path string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func TestUntrackedLineCountIsNotReadAgainForAnUnchangedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lines, ok := countUntrackedTextLines(root, "notes.txt"); !ok || lines != 2 {
		t.Fatalf("first count = %d, %v; want 2, true", lines, ok)
	}
	rewriteKeepingStat(t, path, "abcd")
	if lines, ok := countUntrackedTextLines(root, "notes.txt"); !ok || lines != 2 {
		t.Fatalf("same size and time = %d, %v; want the remembered 2, true (not read again)", lines, ok)
	}
	touchLater(t, path)
	if lines, ok := countUntrackedTextLines(root, "notes.txt"); !ok || lines != 1 {
		t.Fatalf("after a change = %d, %v; want 1, true", lines, ok)
	}
}

func TestUntrackedBinaryFileIsRememberedAsUncountable(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, []byte("\x00\x01\x02\x03"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lines, ok := countUntrackedTextLines(root, "image.png"); ok || lines != 0 {
		t.Fatalf("binary = %d, %v; want 0, false", lines, ok)
	}
	rewriteKeepingStat(t, path, "ab\nc")
	if _, ok := countUntrackedTextLines(root, "image.png"); ok {
		t.Fatal("an unchanged binary file was read again")
	}
}

func TestWorkspaceFileBinaryFlagIsNotReadAgainForAnUnchangedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data.bin")
	if err := os.WriteFile(path, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if size, binary := workspaceFileSizeAndBinary(root, "data.bin", WorkspaceFileAdded); size != 4 || binary {
		t.Fatalf("first = %d, %v; want 4, false", size, binary)
	}
	rewriteKeepingStat(t, path, "\x00ext")
	if _, binary := workspaceFileSizeAndBinary(root, "data.bin", WorkspaceFileAdded); binary {
		t.Fatal("an unchanged file was read again")
	}
	touchLater(t, path)
	if _, binary := workspaceFileSizeAndBinary(root, "data.bin", WorkspaceFileAdded); !binary {
		t.Fatal("a changed file kept its old answer")
	}
}

func TestWorkspaceFileFactsCacheStaysBounded(t *testing.T) {
	cache := newWorkspaceFileFactsCache(2)
	root := t.TempDir()
	infoOf := func(name string) os.FileInfo {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	a, b, c := infoOf("a"), infoOf("b"), infoOf("c")
	cache.setBinary("a", a, false)
	cache.setBinary("b", b, false)
	cache.setBinary("b", b, true) // an update of a known file keeps the others
	if _, known := cache.binary("a", a); !known {
		t.Fatal("updating a known entry dropped another one")
	}
	cache.setBinary("c", c, false) // full: starts over
	if len(cache.entries) != 1 {
		t.Fatalf("entries = %d; want 1 after starting over", len(cache.entries))
	}
	if _, known := cache.binary("a", a); known {
		t.Fatal("the cache grew past its limit")
	}
}
