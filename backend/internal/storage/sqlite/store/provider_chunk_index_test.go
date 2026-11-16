package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// ensureChunkIndex builds the provider-chunk janitor's index, as the janitor's
// first pass does.
func ensureChunkIndex(t *testing.T, s *sqlite.Store) {
	t.Helper()
	ready, err := s.EnsureProviderChunkIndex(context.Background(), nil)
	if err != nil || !ready {
		t.Fatalf("EnsureProviderChunkIndex = %v, %v; want the index built", ready, err)
	}
}

// janitorFixture is conversationFixture with the janitor's index built.
func janitorFixture(t *testing.T) (*sqlite.Store, domain.SessionID, string) {
	t.Helper()
	s, session, conversation := conversationFixture(t)
	ensureChunkIndex(t, s)
	return s, session, conversation
}

func chunkIndexExists(t *testing.T, s *sqlite.Store) bool {
	t.Helper()
	exists, err := s.ProviderChunkIndexExistsForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

// rawDatabase opens the store's database file on a connection of its own.
func rawDatabase(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// The chunk index is built over the whole archive table, the largest in ao.db.
// A migration runs before AO listens: on 2026-11-16 the first start after an
// update that added an index that way was not ready in the launcher's 15
// minutes and was killed. So the migrations leave this index out, and the
// janitor's first pass builds it, over the rows archived before it existed.
func TestProviderChunkIndexIsBuiltAfterStartupNotByAMigration(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	if chunkIndexExists(t, s) {
		t.Fatal("the migrations built the chunk index: that build must stay off AO's startup")
	}
	noop := func(context.Context) error { return nil }
	now := histClock.Add(48 * time.Hour)
	for _, event := range []struct {
		method string
		at     time.Time
	}{
		{"reasoning.delta", now.Add(-30 * time.Hour)},
		{"turn.started", now.Add(-30 * time.Hour)},
		{"message.delta", now.Add(-time.Minute)},
	} {
		if _, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "", event.method, `{"delta":"x"}`, event.at, noop); err != nil {
			t.Fatalf("archive %s: %v", event.method, err)
		}
	}
	// Without the index a janitor read fails; it never walks the archive instead.
	if _, err := s.ProviderEventIDBefore(ctx, 0, now); err == nil || !strings.Contains(err.Error(), "no such index") {
		t.Fatalf("a janitor read without the index returned %v, want it refused for the missing index", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ready, err := s.EnsureProviderChunkIndex(ctx, logger)
	if err != nil || !ready {
		t.Fatalf("EnsureProviderChunkIndex = %v, %v; want the index built", ready, err)
	}
	if !chunkIndexExists(t, s) {
		t.Fatal("the index is not in the schema after the build")
	}
	if got := logs.String(); !strings.Contains(got, "provider chunk index built") || !strings.Contains(got, "chunkRows=2") {
		t.Fatalf("log = %q, want the build reported with its 2 chunk rows", got)
	}
	found, removed, _, err := s.PruneSettledProviderDeltas(ctx, now.Add(-6*time.Hour), now.Add(-24*time.Hour), 0, math.MaxInt64, 100)
	if err != nil || found != 1 || removed != 1 {
		t.Fatalf("prune after the build = found %d removed %d, %v; want the old chunk archived before the index existed", found, removed, err)
	}

	logs.Reset()
	if ready, err := s.EnsureProviderChunkIndex(ctx, logger); err != nil || !ready {
		t.Fatalf("second EnsureProviderChunkIndex = %v, %v", ready, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a store that has the index built again: %q", logs.String())
	}
}

// Coding Tools 0.8.125 shipped migration 9010 while it still built the index.
// An install that ran it has the index; the store finds it and builds nothing.
func TestProviderChunkIndexFromTheOldMigrationIsKept(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	if _, err := rawDatabase(t, dataDir).Exec(`
CREATE INDEX idx_conversation_provider_events_chunks
    ON conversation_provider_events (id, received_at, conversation_id, method)
    WHERE method IN ('reasoning.delta', 'message.delta', 'command.output.delta')`); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	ready, err := s.EnsureProviderChunkIndex(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil || !ready {
		t.Fatalf("EnsureProviderChunkIndex = %v, %v; want the existing index accepted", ready, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("the store built over an index it already had: %q", logs.String())
	}
}

// CREATE INDEX holds the single writer for as long as it runs and cannot be
// split into batches. A build that outlasts its hold is interrupted and rolled
// back whole, the writer is free again, and the next try gets twice the hold.
func TestProviderChunkIndexBuildGivesTheWriterBackAndTriesLonger(t *testing.T) {
	// Enough rows that the build outlasts the 50 ms the store takes to notice.
	const chunkRows = 120_000
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	// This connection does not enforce foreign keys, so the rows need no
	// conversation; they name a branch so no trigger has to look one up.
	if _, err := rawDatabase(t, dataDir).Exec(`
WITH RECURSIVE series(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM series WHERE x < ?)
INSERT INTO conversation_provider_events
    (conversation_id, session_id, provider_event_id, method, payload_json, received_at, branch_id)
SELECT 'conv-bulk', 'session-bulk', '', 'reasoning.delta', '{"delta":"x"}', '2026-01-01 00:00:00 +0000 UTC', 'branch-bulk'
FROM series`, chunkRows); err != nil {
		t.Fatalf("archive %d chunk rows: %v", chunkRows, err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	s.SetProviderChunkIndexHoldsForTest(time.Millisecond, time.Millisecond)
	ready, err := s.EnsureProviderChunkIndex(ctx, logger)
	if err != nil || ready {
		t.Fatalf("EnsureProviderChunkIndex with a 1 ms hold = %v, %v; want not ready and no error", ready, err)
	}
	if chunkIndexExists(t, s) {
		t.Fatal("an interrupted build left the index in the schema")
	}
	if got := logs.String(); !strings.Contains(got, "gave the writer back") {
		t.Fatalf("log = %q, want the interrupted build reported", got)
	}
	if got := s.ProviderChunkIndexHoldForTest(); got != 2*time.Millisecond {
		t.Fatalf("next hold = %s, want twice the one that was not enough", got)
	}
	wrote := make(chan error, 1)
	go func() {
		wrote <- s.UpsertProject(ctx, domain.ProjectRecord{ID: "after", Path: "/tmp/after", RegisteredAt: time.Now().UTC().Truncate(time.Second)})
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("a write after the interrupted build failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted build still holds the writer")
	}

	s.SetProviderChunkIndexHoldsForTest(time.Hour, time.Hour)
	if ready, err := s.EnsureProviderChunkIndex(ctx, logger); err != nil || !ready {
		t.Fatalf("EnsureProviderChunkIndex with room = %v, %v; want the index built", ready, err)
	}
	if !chunkIndexExists(t, s) {
		t.Fatal("the index is not in the schema after the build")
	}
	through, err := s.ProviderEventIDBefore(ctx, 0, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || through != chunkRows {
		t.Fatalf("ProviderEventIDBefore from the new index = %d, %v; want %d", through, err, chunkRows)
	}
}
