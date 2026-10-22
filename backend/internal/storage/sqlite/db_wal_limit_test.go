package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The write-ahead log is cut back to 64 MB when it restarts, so one burst of
// writes does not leave a huge ao.db-wal on disk for good.
func TestConnectionsLimitTheWriteAheadLogSize(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var limit int64
	if err := db.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != 64<<20 {
		t.Fatalf("journal_size_limit = %d, want %d", limit, 64<<20)
	}
}
