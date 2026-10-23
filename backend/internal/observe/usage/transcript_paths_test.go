package usage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTranscriptPathsMatchCanonicalTranscriptPath(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) string {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	plain := write("projects/one/a.jsonl")
	write("projects/one/B.jsonl")
	other := write("projects/two/c.jsonl")
	inputs := []string{
		plain,
		filepath.Join(root, "projects", "one", "B.jsonl"),
		filepath.Join(root, "projects", "one", "b.jsonl"), // another spelling of B.jsonl
		filepath.Join(root, "projects", "one", "gone.jsonl"),
		filepath.Join(root, "projects", "missing-folder", "x.jsonl"),
		"  " + other + "  ",
		filepath.Join(root, "projects", "two", ".", "c.jsonl"),
	}
	if linked := filepath.Join(root, "projects", "one", "link.jsonl"); os.Symlink(other, linked) == nil {
		inputs = append(inputs, linked)
	}
	if runtime.GOOS == "windows" {
		junction := filepath.Join(root, "alias")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, filepath.Join(root, "projects")).CombinedOutput(); err == nil {
			inputs = append(inputs, filepath.Join(junction, "one", "a.jsonl"), filepath.Join(junction, "two", "gone.jsonl"))
		} else {
			t.Logf("no junction case: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	ctx := withTranscriptPaths(context.Background())
	for _, input := range inputs {
		want := canonicalTranscriptPath(input)
		if got := canonicalTranscriptPathIn(ctx, input); got != want {
			t.Errorf("canonicalTranscriptPathIn(%q) = %q, want %q", input, got, want)
		}
	}
	if paths := ctx.Value(transcriptPathsKey{}).(*transcriptPaths); len(paths.dirs) > 6 {
		t.Errorf("resolved %d folders, want each folder once", len(paths.dirs))
	}
	if got := canonicalTranscriptPathIn(context.Background(), plain); got != canonicalTranscriptPath(plain) {
		t.Errorf("without shared paths = %q, want %q", got, canonicalTranscriptPath(plain))
	}
}
