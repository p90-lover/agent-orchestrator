package store

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// A long write while another waits is reported once, naming the holder's store
// method; a short or uncontended hold is not.
func TestWatchWriterNamesALongWriterThatOthersWaitOn(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db, db)
	var out bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	}), nil))
	logged := func() string { mu.Lock(); defer mu.Unlock(); return out.String() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.WatchWriter(ctx, logger, 500*time.Millisecond)

	// Held alone for over a second: nobody waits, so nothing is reported.
	s.writeMu.Lock()
	time.Sleep(1300 * time.Millisecond)
	s.writeMu.Unlock()
	if got := logged(); got != "" {
		t.Fatalf("an uncontended hold was reported: %s", got)
	}

	release := make(chan struct{})
	go s.holdWriterForTest(release)
	time.Sleep(50 * time.Millisecond)
	waited := make(chan struct{})
	go func() { s.writeMu.Lock(); s.writeMu.Unlock(); close(waited) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logged(), "sqlite writer held") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	close(release)
	<-waited
	got := logged()
	if !strings.Contains(got, "sqlite writer held while writes wait") || !strings.Contains(got, "waiting=1") {
		t.Fatalf("the long writer was not reported: %q", got)
	}
	if !strings.Contains(got, "holdWriterForTest") {
		t.Fatalf("the report does not name the holder: %q", got)
	}
	if strings.Count(got, "sqlite writer held") != 1 {
		t.Fatalf("reported more than once a minute: %q", got)
	}
}

// holdWriterForTest is a Store method so the holder's stack names it.
func (s *Store) holdWriterForTest(release <-chan struct{}) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	<-release
}

func TestWriterHoldersSkipsWaitersAndPutsWritersFirst(t *testing.T) {
	stacks := []byte(strings.Join([]string{
		"goroutine 1 [chan receive]:\nx/storage/sqlite/store.(*contextMutex).Lock(...)\nx/storage/sqlite/store.(*Store).CreateSession(...)",
		"goroutine 2 [select]:\nx/storage/sqlite/store.(*Store).ListSessions(...)",
		"goroutine 3 [syscall]:\ndatabase/sql.(*Tx).ExecContext(...)\nx/storage/sqlite/store.(*Store).insertUsage(...)\nx/storage/sqlite/store.(*Store).ReconcileUsage(...)",
	}, "\n\n"))
	if got, want := writerHolders(stacks), "insertUsage<-ReconcileUsage | ListSessions"; got != want {
		t.Fatalf("writerHolders = %q, want %q", got, want)
	}
	if got := writerHolders([]byte("goroutine 9 [running]:\nmain.main()")); !strings.HasPrefix(got, "unknown") {
		t.Fatalf("no store frames should read unknown, got %q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
