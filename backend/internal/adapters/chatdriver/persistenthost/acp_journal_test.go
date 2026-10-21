package persistenthost

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func replayAll(t *testing.T, journal *acpPromptJournal, spans []acpJournalSpan) string {
	t.Helper()
	var replay bytes.Buffer
	for _, span := range spans {
		if err := journal.replayTo(context.Background(), &replay, span); err != nil {
			t.Fatal(err)
		}
	}
	return replay.String()
}

func appendAll(t *testing.T, journal *acpPromptJournal, frames ...string) []acpJournalSpan {
	t.Helper()
	var spans []acpJournalSpan
	for _, frame := range frames {
		span, err := journal.append(context.Background(), []byte(frame))
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span)
	}
	return spans
}

func TestACPPromptJournalReplaysResetsAndEnforcesQuota(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.journal")
	journal, err := openACPPromptJournal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.close(context.Background()) })
	spans := appendAll(t, journal, "one\n", "two\n")
	if got := replayAll(t, journal, spans); got != "one\ntwo\n" {
		t.Fatalf("replay = %q", got)
	}
	if err := journal.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := replayAll(t, journal, []acpJournalSpan{{}}); got != "" {
		t.Fatalf("reset replay = %q", got)
	}
	journal.size = maxACPJournalBytes
	if _, err := journal.append(context.Background(), []byte("overflow\n")); !errors.Is(err, errACPJournalFull) {
		t.Fatalf("quota error = %v", err)
	}
}

// A prompt that fits in memory never touches the disk; one that outgrows it
// moves to an owner-only file that still replays every span, and the file is
// gone again once the prompt ends.
func TestACPPromptJournalWritesItsFileOnlyPastTheMemoryLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.journal")
	if err := os.WriteFile(path, []byte("stale provider output"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := openACPPromptJournal(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.close(context.Background()) })
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a previous host's journal survived open: %v", err)
	}
	journal.memLimit = 8
	spans := appendAll(t, journal, "one\n", "two\n")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an in-memory prompt created its file: %v", err)
	}
	spans = append(spans, appendAll(t, journal, "three\n", "four\n")...)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("a prompt past the memory limit has no file: %v", err)
	}
	if info.Size() != int64(len("one\ntwo\nthree\nfour\n")) {
		t.Fatalf("spilled file size = %d", info.Size())
	}
	if info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("journal permissions = %v", info.Mode().Perm())
	}
	if got := replayAll(t, journal, spans); got != "one\ntwo\nthree\nfour\n" {
		t.Fatalf("replay = %q", got)
	}
	if got := replayAll(t, journal, spans[1:2]); got != "two\n" {
		t.Fatalf("replay of a span from before the spill = %q", got)
	}
	if err := journal.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset left the spilled file: %v", err)
	}
	spans = appendAll(t, journal, "five\n")
	if got := replayAll(t, journal, spans); got != "five\n" {
		t.Fatalf("replay after reset = %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the next small prompt created a file: %v", err)
	}
}
