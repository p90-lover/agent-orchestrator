package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func retentionReaderFixture(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "retention.db")
	writer, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE change_log (seq INTEGER PRIMARY KEY, created_at DATETIME NOT NULL); CREATE INDEX change_log_age ON change_log(created_at, seq);`); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return NewStore(writer, reader)
}

func TestChangeLogNoopRetentionDoesNotQueueOnWriter(t *testing.T) {
	for _, mode := range []string{"age", "size"} {
		t.Run(mode, func(t *testing.T) {
			s := retentionReaderFixture(t)
			if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (1, ?)`, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			conn, err := s.writeDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var n int64
			if mode == "age" {
				n, err = s.PruneChangeLogBefore(ctx, time.Now().Add(-time.Hour), 100)
			} else {
				n, err = s.PruneChangeLogToMaxRows(ctx, 100, 100)
			}
			if err != nil || n != 0 {
				t.Fatalf("no-op %s cleanup queued on foreground writer: removed=%d err=%v", mode, n, err)
			}
			if got := s.writeDB.Stats().WaitCount; got != 0 {
				t.Fatalf("cleanup queued %d writer requests", got)
			}
		})
	}
}

func TestChangeLogRetentionKeepsNewestRowsAcrossSequenceGaps(t *testing.T) {
	s := retentionReaderFixture(t)
	for _, seq := range []int{1, 4, 20, 90, 1000} {
		if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (?, ?)`, seq, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []int64{1, 1, 0} {
		n, err := s.PruneChangeLogToMaxRows(context.Background(), 3, 1)
		if err != nil || n != want {
			t.Fatalf("batch %d removed=%d err=%v, want %d", i, n, err, want)
		}
	}
	rows, err := s.readDB.Query(`SELECT seq FROM change_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		got = append(got, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[20 90 1000]" {
		t.Fatalf("remaining rows=%v", got)
	}
}

func TestChangeLogRetentionInvalidBoundsDoNotDelete(t *testing.T) {
	s := retentionReaderFixture(t)
	if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (1, ?)`, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, bounds := range [][2]int64{{3, 0}, {3, -1}, {-1, 100}} {
		if n, err := s.PruneChangeLogToMaxRows(context.Background(), bounds[0], bounds[1]); err != nil || n != 0 {
			t.Fatalf("bounds=%v n=%d err=%v", bounds, n, err)
		}
	}
	for _, limit := range []int64{0, -1} {
		if n, err := s.PruneChangeLogBefore(context.Background(), time.Now(), limit); err != nil || n != 0 {
			t.Fatalf("limit=%d n=%d err=%v", limit, n, err)
		}
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestChangeLogRetentionAgeOrderAndZeroCap(t *testing.T) {
	s := retentionReaderFixture(t)
	now := time.Now().UTC()
	for _, row := range []struct {
		seq int
		at  time.Time
	}{{1, now.Add(-time.Hour)}, {4, now.Add(-48 * time.Hour)}, {20, now}} {
		if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (?, ?)`, row.seq, row.at); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.PruneChangeLogBefore(context.Background(), now, 1); err != nil || n != 1 {
		t.Fatalf("age n=%d err=%v", n, err)
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM change_log WHERE seq = 4`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("oldest row count=%d err=%v", count, err)
	}
	for i := 0; i < 2; i++ {
		if n, err := s.PruneChangeLogToMaxRows(context.Background(), 0, 1); err != nil || n != 1 {
			t.Fatalf("zero cap batch %d n=%d err=%v", i, n, err)
		}
	}
	if n, err := s.PruneChangeLogToMaxRows(context.Background(), 0, 1); err != nil || n != 0 {
		t.Fatalf("empty n=%d err=%v", n, err)
	}
}

func TestChangeLogConcurrentRetentionKeepsSelectedBoundary(t *testing.T) {
	s := retentionReaderFixture(t)
	for _, seq := range []int{1, 4, 20, 90, 1000} {
		if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (?, ?)`, seq, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.writeMu.Lock()
	type outcome struct {
		n   int64
		err error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() { n, err := s.PruneChangeLogToMaxRows(ctx, 3, 2); results <- outcome{n, err} }()
	}
	for s.writeMu.waiting.Load() < 2 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	waiting := s.writeMu.waiting.Load()
	_, insertErr := s.writeDB.Exec(`INSERT INTO change_log VALUES (10000, ?)`, time.Now().UTC())
	s.writeMu.Unlock()
	if insertErr != nil {
		t.Fatal(insertErr)
	}
	if waiting != 2 {
		t.Fatalf("retention did not finish discovery before writer: waiting=%d", waiting)
	}
	var removed int64
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			removed += r.n
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if removed != 2 {
		t.Fatalf("concurrent passes removed=%d, want same two selected rows", removed)
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM change_log WHERE seq >= 20`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("newer count=%d err=%v", count, err)
	}
}

func TestChangeLogRetentionCanceledWhileWaitingForWriter(t *testing.T) {
	s := retentionReaderFixture(t)
	if _, err := s.writeDB.Exec(`INSERT INTO change_log VALUES (1, ?)`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.PruneChangeLogToMaxRows(ctx, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait err=%v", err)
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("canceled delete count=%d err=%v", count, err)
	}
}

func TestChangeLogRetentionCanceledContext(t *testing.T) {
	s := retentionReaderFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.PruneChangeLogToMaxRows(ctx, 3, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("size err=%v", err)
	}
	if _, err := s.PruneChangeLogBefore(ctx, time.Now(), 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("age err=%v", err)
	}
}
