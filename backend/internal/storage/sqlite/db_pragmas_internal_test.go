package sqlite

import (
	"database/sql"
	"testing"
)

// Every writable connection caps the WAL file it leaves behind after a
// checkpoint, and still runs in WAL mode.
func TestWritableConnectionsCapTheWALFile(t *testing.T) {
	db, err := sql.Open("sqlite", databaseURI(t.TempDir())+pragmas)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var limit int64
	if err := db.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil {
		t.Fatalf("journal_size_limit: %v", err)
	}
	if limit != 64<<20 {
		t.Fatalf("journal_size_limit = %d, want %d", limit, 64<<20)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}
