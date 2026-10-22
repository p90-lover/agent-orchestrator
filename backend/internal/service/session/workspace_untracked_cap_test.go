package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListedWorkspaceChangesKeepsTheFirstSortedPaths(t *testing.T) {
	statuses := map[string]WorkspaceFileStatus{"c": WorkspaceFileAdded, "a": WorkspaceFileAdded, "b": WorkspaceFileModified}
	counts := map[string][2]int{"b": {1, 0}, "d": {2, 0}}
	if listed := listedWorkspaceChanges(statuses, counts, 4); listed != nil {
		t.Fatalf("all four fit: got %v, want nil", listed)
	}
	listed := listedWorkspaceChanges(statuses, counts, 2)
	if len(listed) != 2 {
		t.Fatalf("listed = %v; want a and b", listed)
	}
	for _, rel := range []string{"a", "b"} {
		if _, ok := listed[rel]; !ok {
			t.Fatalf("listed = %v; missing %q", listed, rel)
		}
	}
}

func TestUntrackedSummariesStopAtTheLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("one\ntwo\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{"c.txt", "a.txt", "b.txt"}
	files, truncated := buildUntrackedSummaries(root, paths, 2)
	if !truncated || len(files) != 2 || files[0].Path != "a.txt" || files[1].Path != "b.txt" {
		t.Fatalf("limit 2 = %+v, truncated %v; want a.txt, b.txt, truncated", files, truncated)
	}
	if files[0].Additions != 2 {
		t.Fatalf("a.txt additions = %d; want 2", files[0].Additions)
	}
	files, truncated = buildUntrackedSummaries(root, paths, 3)
	if truncated || len(files) != 3 {
		t.Fatalf("limit 3 = %d files, truncated %v; want 3, false", len(files), truncated)
	}
}
