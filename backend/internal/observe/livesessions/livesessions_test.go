package livesessions

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type countingSource struct {
	calls   atomic.Int32
	err     error
	release chan struct{} // when set, each read waits for it
	started chan struct{} // when set, receives once per read as it begins
	records []domain.SessionRecord
}

func (s *countingSource) ListLiveSessions(context.Context) ([]domain.SessionRecord, error) {
	s.calls.Add(1)
	if s.started != nil {
		s.started <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.records, nil
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestCallersWithinMaxAgeShareOneRead(t *testing.T) {
	src := &countingSource{records: []domain.SessionRecord{{ID: "a-1"}, {ID: "a-2"}}}
	clock := &fakeClock{now: time.Unix(1000, 0)}
	snap := New(src, Config{MaxAge: 4 * time.Second, Clock: clock.Now})
	ctx := context.Background()

	for range 3 { // reaper, then two observers in the same window
		got, err := snap.ListLiveSessions(ctx)
		if err != nil || len(got) != 2 {
			t.Fatalf("got %v, %v", got, err)
		}
		clock.Add(time.Second)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("reads = %d, want 1 within MaxAge", n)
	}
	clock.Add(time.Second) // 4s after the read
	if _, err := snap.ListLiveSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("reads = %d, want a fresh read once MaxAge has passed", n)
	}
}

func TestEachCallerGetsItsOwnSlice(t *testing.T) {
	src := &countingSource{records: []domain.SessionRecord{{ID: "a-1"}, {ID: "a-2"}}}
	snap := New(src, Config{})
	first, _ := snap.ListLiveSessions(context.Background())
	first[0] = domain.SessionRecord{ID: "changed"}
	_ = append(first[:1], domain.SessionRecord{ID: "appended"})
	second, _ := snap.ListLiveSessions(context.Background())
	if second[0].ID != "a-1" || second[1].ID != "a-2" {
		t.Fatalf("a caller's edits reached another caller: %v", second)
	}
}

func TestConcurrentCallersWaitForTheReadInFlight(t *testing.T) {
	src := &countingSource{records: []domain.SessionRecord{{ID: "a-1"}}, release: make(chan struct{}), started: make(chan struct{}, 1)}
	snap := New(src, Config{})
	var wg sync.WaitGroup
	results := make(chan int, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := snap.ListLiveSessions(context.Background())
			if err != nil {
				t.Error(err)
			}
			results <- len(got)
		}()
	}
	<-src.started
	time.Sleep(20 * time.Millisecond) // let the others queue on the read
	close(src.release)
	wg.Wait()
	close(results)
	for n := range results {
		if n != 1 {
			t.Fatalf("a caller got %d records", n)
		}
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("reads = %d, want the callers to share one", n)
	}
}

func TestAFailedReadIsNotKept(t *testing.T) {
	src := &countingSource{err: errors.New("database is locked")}
	snap := New(src, Config{})
	if _, err := snap.ListLiveSessions(context.Background()); err == nil {
		t.Fatal("want the read's error")
	}
	src.err = nil
	src.records = []domain.SessionRecord{{ID: "a-1"}}
	got, err := snap.ListLiveSessions(context.Background())
	if err != nil || len(got) != 1 || src.calls.Load() != 2 {
		t.Fatalf("got %v, %v after %d reads; want a fresh read after a failure", got, err, src.calls.Load())
	}
}

func TestInvalidateForcesAFreshReadEvenOverOneInFlight(t *testing.T) {
	src := &countingSource{records: []domain.SessionRecord{{ID: "a-1"}}}
	snap := New(src, Config{MaxAge: time.Hour})
	ctx := context.Background()
	_, _ = snap.ListLiveSessions(ctx)
	snap.Invalidate()
	_, _ = snap.ListLiveSessions(ctx)
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("reads = %d, want Invalidate to force one", n)
	}

	// A read begun before Invalidate is neither joined nor kept.
	src.release = make(chan struct{})
	src.started = make(chan struct{}, 2)
	snap.Invalidate()
	early := make(chan struct{})
	go func() { _, _ = snap.ListLiveSessions(ctx); close(early) }()
	<-src.started
	snap.Invalidate()
	late := make(chan struct{})
	go func() { _, _ = snap.ListLiveSessions(ctx); close(late) }()
	<-src.started // the late caller started its own read
	close(src.release)
	<-early
	<-late
	src.release = nil
	src.started = nil
	if n := src.calls.Load(); n != 4 {
		t.Fatalf("reads = %d, want the caller after Invalidate to read afresh", n)
	}
	_, _ = snap.ListLiveSessions(ctx)
	if n := src.calls.Load(); n != 4 {
		t.Fatalf("reads = %d, want the post-Invalidate read kept", n)
	}
}

func TestACancelledWaiterDoesNotFailTheRead(t *testing.T) {
	src := &countingSource{records: []domain.SessionRecord{{ID: "a-1"}}, release: make(chan struct{}), started: make(chan struct{}, 1)}
	snap := New(src, Config{})
	reader := make(chan error, 1)
	go func() {
		_, err := snap.ListLiveSessions(context.Background())
		reader <- err
	}()
	<-src.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snap.ListLiveSessions(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v", err)
	}
	close(src.release)
	if err := <-reader; err != nil {
		t.Fatalf("the read failed for its own caller: %v", err)
	}
}
