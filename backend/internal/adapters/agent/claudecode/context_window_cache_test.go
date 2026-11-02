package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A launch that sets a context window must not run `claude --version` again for
// a binary it already checked: the answer is reused until the file changes.
func TestClaudeVersionIsReusedUntilTheBinaryChanges(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "claude-not-executable")
	if err := os.WriteFile(binary, []byte("not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatal(err)
	}
	versionCache.Store(versionKey{path: binary, size: info.Size(), modTime: info.ModTime().UnixNano()}, "2.1.200 (Claude Code)")

	got, err := claudeVersion(context.Background(), binary)
	if err != nil || got != "2.1.200 (Claude Code)" {
		t.Fatalf("cached version = %q, %v; want the stored answer without running the file", got, err)
	}

	// An update rewrites the binary: the old answer no longer applies, so the
	// probe runs (and fails here, because the file is not a program).
	later := info.ModTime().Add(time.Minute)
	if err := os.Chtimes(binary, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeVersion(context.Background(), binary); err == nil {
		t.Fatal("a changed binary reused the cached version instead of probing again")
	}
}
