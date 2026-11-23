package sqlite

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// A database a release behind has migrations to run before the daemon listens;
// one of them (9009) built an index for 15 minutes on a 4.7 GB ao.db while the
// daemon printed nothing. Open with ReportStages names each one as it begins,
// in the order goose applies them, so the daemon's progress lines say which.
func TestOpenReportsEachPendingMigrationBeforeItRuns(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", databaseURI(dataDir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	upTo(t, db, 9007)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stages []string
	check, err := sql.Open("sqlite", databaseURI(dataDir)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	applied := func(file string) int {
		var version int64
		if _, err := fmt.Sscanf(file, "%d_", &version); err != nil {
			t.Fatalf("migration file %q: %v", file, err)
		}
		var rows int
		if err := check.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?`, version).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	stage := func(name string, attrs ...any) {
		entry := name
		for i := 0; i+1 < len(attrs); i += 2 {
			entry += fmt.Sprintf(" %v=%v", attrs[i], attrs[i+1])
			if attrs[i] == "migration" {
				// Reported before it runs: the ledger does not have it yet.
				entry += fmt.Sprintf(" applied=%d", applied(attrs[i+1].(string)))
			}
		}
		stages = append(stages, entry)
	}

	store, err := Open(dataDir, ReportStages(stage))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"prepare migrations",
		"migrate migration=9008_conversation_turns_queue_head_index.sql applied=0 left=4",
		"migrate migration=9009_conversation_activity_page_keys_index.sql applied=0 left=3",
		"migrate migration=9010_conversation_provider_events_chunks_index.sql applied=0 left=2",
		"migrate migration=9011_conversation_turns_floor_sequence.sql applied=0 left=1",
		"reconcile schema",
	}
	if strings.Join(stages, "\n") != strings.Join(want, "\n") {
		t.Fatalf("stages:\n%s\nwant:\n%s", strings.Join(stages, "\n"), strings.Join(want, "\n"))
	}
	if applied("9011_x.sql") != 1 {
		t.Fatal("the last migration ran after it was reported")
	}
}

// A database that is up to date reports no migration, and Open without
// ReportStages reports nothing at all.
func TestOpenReportsNoMigrationWhenNoneIsPending(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stages []string
	store, err = Open(dataDir, ReportStages(func(name string, attrs ...any) { stages = append(stages, name) }))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stages, ",") != "prepare migrations,reconcile schema" {
		t.Fatalf("stages: %v", stages)
	}
}
