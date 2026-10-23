//go:build windows

package fspath

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveWalksThroughAJunction(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "moved", "data")
	file := filepath.Join(target, "sessions", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "home", ".ao")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v: %s", err, out)
	}
	throughLink := filepath.Join(link, "sessions", "rollout.jsonl")
	if _, err := filepath.EvalSymlinks(throughLink); err == nil {
		t.Log("filepath.EvalSymlinks walks junctions again; Resolve's fallback is now unused")
	}

	got, err := Resolve(throughLink)
	if err != nil {
		t.Fatalf("Resolve through junction: %v", err)
	}
	if got != file {
		t.Fatalf("Resolve through junction = %q, want %q", got, file)
	}
	gotRoot, err := Resolve(filepath.Join(link, "sessions"))
	if err != nil || gotRoot != filepath.Dir(file) {
		t.Fatalf("Resolve of a folder through junction = %q, %v; want %q", gotRoot, err, filepath.Dir(file))
	}

	// A path EvalSymlinks already resolves keeps EvalSymlinks' exact answer.
	plain, err := Resolve(file)
	if err != nil || plain != file {
		t.Fatalf("Resolve(%q) = %q, %v", file, plain, err)
	}

	// A missing path still fails with a not-exist error.
	if _, err := Resolve(filepath.Join(link, "sessions", "gone.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("Resolve of a missing path err = %v, want not-exist", err)
	}
}
