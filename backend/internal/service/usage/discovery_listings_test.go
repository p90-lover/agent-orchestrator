package usage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoveryPassGlobMatchesFilepathGlobAndListsEachFolderOnce(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"2026/10/22/rollout-a-019a.jsonl",
		"2026/10/23/rollout-b-019b.jsonl",
		"2026/10/23/rollout-c-019a.jsonl",
		"2026/10/23/notes.txt",
		"2025/12/31/rollout-d-019a.jsonl",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patterns := []string{
		filepath.Join(root, "*", "*", "*", "*019a*.jsonl"),
		filepath.Join(root, "*", "*", "*", "*019b*.jsonl"),
		filepath.Join(root, "2026", "10", "23", "*.jsonl"),
		filepath.Join(root, "2026", "10", "23", "notes.txt"),
		filepath.Join(root, "*", "missing.jsonl"),
		filepath.Join(root, "absent", "*", "*.jsonl"),
	}
	plain := context.Background()
	ctx := withDiscoveryPass(plain)
	for _, pattern := range patterns {
		want, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		got, err := discoveryGlob(ctx, pattern)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("glob(%s) = %v, want %v", pattern, got, want)
		}
	}
	// A file created mid-pass is not seen until the next pass: the folder was
	// listed once, which is the point.
	late := filepath.Join(root, "2026", "10", "23", "rollout-e-019a.jsonl")
	if err := os.WriteFile(late, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := discoveryGlob(ctx, patterns[0]); len(got) != 3 {
		t.Fatalf("mid-pass glob = %v, want the 3 listed at the start of the pass", got)
	}
	if got, _ := discoveryGlob(plain, patterns[0]); len(got) != 4 {
		t.Fatalf("glob outside the pass = %v, want all 4", got)
	}
	if got, _ := discoveryGlob(withDiscoveryPass(plain), patterns[0]); len(got) != 4 {
		t.Fatalf("glob in the next pass = %v, want all 4", got)
	}
	if _, err := discoveryGlob(ctx, filepath.Join(root, "[")); err == nil {
		t.Fatal("a malformed pattern must fail during a pass as filepath.Glob does")
	}

	// Resolution is remembered for the pass, errors included.
	missing := filepath.Join(root, "gone.jsonl")
	if _, err := discoveryEvalSymlinks(ctx, missing); err == nil {
		t.Fatal("resolving a missing path must fail")
	}
	if err := os.WriteFile(missing, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := discoveryEvalSymlinks(ctx, missing); err == nil {
		t.Fatal("the pass must keep the resolution it made first")
	}
	want, err := filepath.EvalSymlinks(missing)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := discoveryEvalSymlinks(plain, missing); err != nil || got != want {
		t.Fatalf("resolution outside the pass = %q, %v; want %q", got, err, want)
	}
}
