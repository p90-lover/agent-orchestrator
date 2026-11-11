package sqlite

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Commits never copy the write-ahead log into the database themselves; the
// daemon's checkpointer does.
func TestConnectionsLeaveCheckpointsToTheCheckpointer(t *testing.T) {
	db, err := sql.Open("sqlite", databaseURI(t.TempDir())+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pages int64
	if err := db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 0 {
		t.Fatalf("wal_autocheckpoint = %d, want 0", pages)
	}
}

// A daemon that is killed leaves its write-ahead log for the next one. The next
// daemon's first write, made before it listens, must not copy that log into the
// database: SQLite's automatic checkpoint did, over the whole log (483 MB on
// 2026-11-11), and the launcher killed the silent daemon over and over.
func TestFirstWriteAfterOpenLeavesALeftoverLogToTheCheckpointer(t *testing.T) {
	killed := t.TempDir()
	first, err := Open(killed)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	dsn := databaseURI(killed) + pragmas
	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`CREATE TABLE fill (id INTEGER PRIMARY KEY, body BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	// An overlapping read keeps the log from being copied as it grows, as the
	// chats' reads did.
	readers, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	read, err := readers.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := read.QueryRow(`SELECT count(*) FROM fill`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	const rows = 1500
	for i := 0; i < rows; i++ {
		if _, err := writer.Exec(`INSERT INTO fill (body) VALUES (randomblob(4096))`); err != nil {
			t.Fatal(err)
		}
	}
	// The files as a kill leaves them: nothing closed, so no closing checkpoint.
	next := t.TempDir()
	for _, name := range []string{"ao.db", "ao.db-wal"} {
		data, err := os.ReadFile(filepath.Join(killed, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(next, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = read.Rollback()
	before, err := os.ReadFile(filepath.Join(next, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}

	// The next daemon: Open (migrations), then a first write on the writer.
	second, err := Open(next)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	again, err := sql.Open("sqlite", databaseURI(next)+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.SetMaxOpenConns(1)
	if _, err := again.Exec(`INSERT INTO fill (body) VALUES (x'00')`); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(next, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("opening and the first write copied the leftover log into ao.db (%d -> %d bytes)", len(before), len(after))
	}
	// The leftover log is all still there, for the checkpointer to copy.
	var busy, frames, copied int64
	if err := again.QueryRow("PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &frames, &copied); err != nil {
		t.Fatal(err)
	}
	if busy != 0 || frames < rows || copied != frames {
		t.Fatalf("checkpoint after the first write = (busy %d, frames %d, copied %d), want at least %d frames, all copied by it", busy, frames, copied, rows)
	}
}
