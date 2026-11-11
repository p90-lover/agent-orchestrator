package store

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// walFixture opens a store the way Open does (one writer, a reader pool, a
// checkpoint connection, no automatic checkpoints), with a table to fill. It
// returns the log's path.
func walFixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ao.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(0)"
	open := func(conns int) *sql.DB {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(conns)
		return db
	}
	writer, reader, checkpointer := open(1), open(8), open(1)
	if _, err := writer.Exec(`CREATE TABLE fill (id INTEGER PRIMARY KEY, body BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	s := NewStore(writer, reader)
	s.UseCheckpointConnection(checkpointer)
	t.Cleanup(func() { _ = s.Close() })
	return s, path + "-wal"
}

// fillWAL commits rows of 4 KB one at a time, as streaming chats do.
func fillWAL(t *testing.T, s *Store, rows int) {
	t.Helper()
	for i := 0; i < rows; i++ {
		if _, err := s.writeDB.Exec(`INSERT INTO fill (body) VALUES (randomblob(4096))`); err != nil {
			t.Fatal(err)
		}
	}
}

func walSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// pinLog starts a read on the reader pool that keeps its snapshot, as an
// overlapping read does.
func pinLog(t *testing.T, s *Store) *sql.Tx {
	t.Helper()
	tx, err := s.readDB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM fill`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return tx
}

func countRows(t *testing.T, s *Store) int {
	t.Helper()
	var rows int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM fill`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// With automatic checkpoints off, commits leave every frame in the log; a pass
// copies them while a write holds the writer, so it never waits on one.
func TestCheckpointWALCopiesTheLogWithoutTheWriter(t *testing.T) {
	s, wal := walFixture(t)
	fillWAL(t, s, 1200)
	if err := s.writeMu.LockContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	pass, err := s.checkpointWAL(context.Background(), wal, 64<<20)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if pass.frames < 1200 || pass.copied != pass.frames || pass.truncated {
		t.Fatalf("pass = %+v, want all of at least 1200 frames copied and no truncation", pass)
	}
	if got := s.writeDB.Stats().WaitCount; got != 0 {
		t.Fatalf("a passive pass queued %d writer requests", got)
	}
}

// Frames a running read still needs stay in the log: the pass returns while the
// read is still open (it does not wait for it) and does not try to empty the log.
func TestCheckpointWALLeavesFramesAReadStillNeeds(t *testing.T) {
	s, wal := walFixture(t)
	fillWAL(t, s, 10)
	reader := pinLog(t, s)
	defer func() { _ = reader.Rollback() }()
	fillWAL(t, s, 400)
	pass, err := s.checkpointWAL(context.Background(), wal, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pass.copied >= pass.frames || pass.truncated || pass.walBytes != 0 {
		t.Fatalf("pass = %+v, want frames left for the read and no truncation attempt", pass)
	}
}

// A log that outgrew the limit while reads held it is emptied once every frame
// is in the database; the rows stay.
func TestCheckpointWALEmptiesAFullyCopiedLogPastTheLimit(t *testing.T) {
	s, wal := walFixture(t)
	fillWAL(t, s, 10)
	reader := pinLog(t, s)
	fillWAL(t, s, 1500)
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	before := walSize(t, wal)
	if before < 4<<20 {
		t.Fatalf("fixture log is %d bytes, want it past 4 MB", before)
	}
	pass, err := s.checkpointWAL(context.Background(), wal, 1<<20)
	if err != nil || !pass.truncated || pass.walBytes != before {
		t.Fatalf("pass = %+v, %v; want the %d-byte log truncated", pass, err, before)
	}
	if got := walSize(t, wal); got != 0 {
		t.Fatalf("log is %d bytes after the pass, want 0", got)
	}
	if rows := countRows(t, s); rows != 1510 {
		t.Fatalf("rows after truncation = %d, want 1510", rows)
	}

	// A small log is left alone, and the writer is not taken for it.
	fillWAL(t, s, 5)
	waits := s.writeDB.Stats().WaitCount
	small := walSize(t, wal)
	if pass, err := s.checkpointWAL(context.Background(), wal, 64<<20); err != nil || pass.truncated {
		t.Fatalf("small log pass = %+v, %v", pass, err)
	}
	if got := walSize(t, wal); got != small {
		t.Fatalf("small log changed from %d to %d bytes", small, got)
	}
	if got := s.writeDB.Stats().WaitCount; got != waits {
		t.Fatalf("a pass with nothing to empty queued %d writer requests", got-waits)
	}
}

// A read still on a fully copied log makes the truncation give up after
// walTruncateWait rather than hang; the writer gets its own busy timeout back,
// and a pass after the read ends empties the log.
func TestCheckpointWALTruncationGivesUpOnAReadAndRetries(t *testing.T) {
	s, wal := walFixture(t)
	fillWAL(t, s, 400)
	reader := pinLog(t, s)
	defer func() { _ = reader.Rollback() }()
	pass, err := s.checkpointWAL(context.Background(), wal, 1<<20)
	if err != nil || pass.truncated || pass.walBytes == 0 || pass.copied != pass.frames {
		t.Fatalf("pass with a read on the log = %+v, %v; want a copied log that could not be emptied", pass, err)
	}
	// With every frame copied, the truncation itself only waits for the read:
	// walTruncateWait, not the writer's own 5 s busy timeout.
	started := time.Now()
	truncated, err := s.truncateWAL(context.Background())
	took := time.Since(started)
	if err != nil || truncated {
		t.Fatalf("truncation with a read on the log = %v, %v; want it to give up", truncated, err)
	}
	if took < walTruncateWait-500*time.Millisecond || took >= 5*time.Second {
		t.Fatalf("truncation gave up after %v, want about %v", took, walTruncateWait)
	}
	var timeout int64
	if err := s.writeDB.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("writer busy_timeout after the pass = %d (%v), want 5000", timeout, err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if pass, err := s.checkpointWAL(context.Background(), wal, 1<<20); err != nil || !pass.truncated {
		t.Fatalf("pass after the read ended = %+v, %v; want the log emptied", pass, err)
	}
	if got := walSize(t, wal); got != 0 {
		t.Fatalf("log is %d bytes after the retry, want 0", got)
	}
}

// Without a checkpoint connection a pass borrows a reader.
func TestCheckpointWALBorrowsAReaderWithoutItsOwnConnection(t *testing.T) {
	s, wal := walFixture(t)
	_ = s.checkpointDB.Close()
	s.checkpointDB = nil
	fillWAL(t, s, 50)
	pass, err := s.checkpointWAL(context.Background(), wal, 64<<20)
	if err != nil || pass.frames < 50 || pass.copied != pass.frames {
		t.Fatalf("pass = %+v, %v; want every frame copied", pass, err)
	}
}

// CheckpointWAL passes at once and on its interval, reports what it emptied,
// and returns when ctx ends.
func TestCheckpointWALRunsUntilCancelled(t *testing.T) {
	s, wal := walFixture(t)
	fillWAL(t, s, 10)
	reader := pinLog(t, s)
	fillWAL(t, s, 400)
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	// Read only after CheckpointWAL has returned.
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.CheckpointWAL(ctx, logger, wal, 1<<20, 10*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for walSize(t, wal) != 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("log still %d bytes after 10 s of passes", walSize(t, wal))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CheckpointWAL did not return after its context ended")
	}
	if text := logs.String(); !strings.Contains(text, "sqlite write-ahead log truncated") {
		t.Fatalf("no truncation report in %q", text)
	}
}
