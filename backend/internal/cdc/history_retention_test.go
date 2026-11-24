package cdc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// historyStore fakes a table of rows 1..archiveEnd (archive) and 1..cardEnd
// (cards) that are all old enough, and a free page count.
type historyStore struct {
	cursors              map[string]int64
	cursorReads          int
	archiveEnd, cardEnd  int64
	archiveCalls         [][3]int64 // after, through, limit
	cardCalls            [][3]int64
	yieldArchiveAt       int // call index that yields; -1 none
	free, pageSize       int64
	incremental          bool
	vacuumSteps          []int64
	yieldVacuum          bool
	order                []string
	archiveCut, cardsCut time.Time
	maxText              int
}

func newHistoryStore() *historyStore {
	return &historyStore{cursors: map[string]int64{}, yieldArchiveAt: -1, pageSize: 4096, incremental: true}
}

func (f *historyStore) RetentionCursor(_ context.Context, name string) (int64, error) {
	f.cursorReads++
	return f.cursors[name], nil
}

func (f *historyStore) SetRetentionCursor(_ context.Context, name string, position int64, _ time.Time) error {
	if position > f.cursors[name] {
		f.cursors[name] = position
	}
	return nil
}

func (f *historyStore) ProviderEventIDReceivedBefore(_ context.Context, after int64, cutoff time.Time) (int64, error) {
	f.archiveCut = cutoff
	return max(after, f.archiveEnd), nil
}

// batch returns the rows in (after, through] up to limit, as a real store does.
func batch(after, through, limit int64) (found, last int64) {
	last = min(through, after+limit)
	return last - after, last
}

func (f *historyStore) SweepProviderEventArchive(_ context.Context, _ time.Time, after, through, limit int64) (int64, int64, int64, int64, error) {
	f.order = append(f.order, "archive")
	if len(f.archiveCalls) == f.yieldArchiveAt {
		f.archiveCalls = append(f.archiveCalls, [3]int64{after, through, limit})
		return 0, 0, 0, 0, ports.ErrMaintenanceYielded
	}
	f.archiveCalls = append(f.archiveCalls, [3]int64{after, through, limit})
	found, last := batch(after, through, limit)
	return found, found, 0, last, nil
}

func (f *historyStore) ActivityRowIDCreatedBefore(_ context.Context, after int64, cutoff time.Time) (int64, error) {
	f.cardsCut = cutoff
	return max(after, f.cardEnd), nil
}

func (f *historyStore) TrimActivityBodies(_ context.Context, _ time.Time, after, through, limit int64, maxText int) (int64, int64, int64, error) {
	f.order = append(f.order, "cards")
	f.maxText = maxText
	f.cardCalls = append(f.cardCalls, [3]int64{after, through, limit})
	found, last := batch(after, through, limit)
	return found, found, last, nil
}

func (f *historyStore) FreePages(context.Context) (int64, int64, bool, error) {
	return f.free, f.pageSize, f.incremental, nil
}

func (f *historyStore) IncrementalVacuumStep(_ context.Context, pages int64) (int64, error) {
	f.order = append(f.order, "vacuum")
	if f.yieldVacuum {
		return 0, ports.ErrMaintenanceYielded
	}
	f.vacuumSteps = append(f.vacuumSteps, pages)
	f.free -= pages
	return pages, nil
}

func historyRetentionFor(f *historyStore) *HistoryRetention {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	return NewHistoryRetention(f, HistoryRetentionConfig{
		ArchiveBatch: 10, CardBatch: 4, VacuumStep: 256, VacuumPassBytes: 4 << 20, VacuumKeepBytes: 1 << 20,
		Pause: -1, StartDelay: -1, Clock: func() time.Time { return now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// One pass: the archive sweep, then the card trim, then vacuum steps, each with
// its own cutoff; the sweeps record how far they got and the next pass, like
// the next AO start, continues from there.
func TestHistoryRetentionPassSweepsTrimsThenVacuums(t *testing.T) {
	f := newHistoryStore()
	f.archiveEnd, f.cardEnd, f.free = 25, 9, 3000 // 3000 pages = 11.7 MB free
	h := historyRetentionFor(f)
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(f.archiveCalls), "[[0 25 10] [10 25 10] [20 25 10]]"; got != want {
		t.Fatalf("archive batches = %s; want %s", got, want)
	}
	if got, want := fmt.Sprint(f.cardCalls), "[[0 9 4] [4 9 4] [8 9 4]]"; got != want {
		t.Fatalf("card batches = %s; want %s", got, want)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if !f.archiveCut.Equal(now.Add(-7*24*time.Hour)) || !f.cardsCut.Equal(now.Add(-30*24*time.Hour)) || f.maxText != 4096 {
		t.Fatalf("cutoffs %s / %s, max text %d; want a week, 30 days, 4 KB", f.archiveCut, f.cardsCut, f.maxText)
	}
	// 4 MB a pass in 1 MB steps, keeping 1 MB (256 pages) free.
	if got, want := fmt.Sprint(f.vacuumSteps), "[256 256 256 256]"; got != want {
		t.Fatalf("vacuum steps = %s; want %s", got, want)
	}
	if f.cursors[historyArchiveCursor] != 25 || f.cursors[historyCardCursor] != 9 {
		t.Fatalf("cursors = %v; want archive 25, cards 9", f.cursors)
	}
	if !h.more || h.wait() != DefaultHistoryBacklogRetry {
		t.Fatalf("free pages over the pass budget: more=%v wait=%s; want a sooner pass", h.more, h.wait())
	}

	// New old rows arrive; the next pass starts where this one stopped.
	f.archiveEnd, f.cardEnd = 31, 9
	f.archiveCalls, f.cardCalls, f.vacuumSteps = nil, nil, nil
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(f.archiveCalls), "[[25 31 10]]"; got != want {
		t.Fatalf("second pass archive batches = %s; want %s", got, want)
	}
	if len(f.cardCalls) != 0 {
		t.Fatalf("second pass read cards again: %v", f.cardCalls)
	}
	if f.cursorReads != 2 {
		t.Fatalf("cursor reads = %d; want one per sweep for the worker's life", f.cursorReads)
	}

	// A restarted worker reads the recorded cursors.
	f.archiveCalls = nil
	again := historyRetentionFor(f)
	if err := again.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.archiveCalls) != 0 {
		t.Fatalf("after a restart the archive was read again: %v", f.archiveCalls)
	}
	// Vacuum stops at the space it keeps free.
	if f.free != 256 || again.more {
		t.Fatalf("free pages = %d, more=%v; want 256 kept and nothing left", f.free, again.more)
	}
}

// A batch that gave the writer to a chat write ends the pass where it was: the
// cursor keeps the finished batches, later steps wait, the next pass comes after
// yieldRetry with a smaller batch.
func TestHistoryRetentionYieldEndsThePassAndResumes(t *testing.T) {
	f := newHistoryStore()
	f.archiveEnd, f.cardEnd, f.free = 25, 9, 3000
	f.yieldArchiveAt = 1
	h := historyRetentionFor(f)
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cursors[historyArchiveCursor] != 10 {
		t.Fatalf("archive cursor = %d; want 10, the finished batch", f.cursors[historyArchiveCursor])
	}
	if len(f.cardCalls) != 0 || len(f.vacuumSteps) != 0 {
		t.Fatal("the pass went on after yielding the writer")
	}
	if h.wait() != yieldRetry || !h.archive.size.again {
		t.Fatalf("wait = %s; want %s", h.wait(), yieldRetry)
	}
	f.yieldArchiveAt = -1
	f.archiveCalls = nil
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.archiveCalls[0][0] != 10 || f.cursors[historyArchiveCursor] != 25 || f.cursors[historyCardCursor] != 9 {
		t.Fatalf("resume = %v, cursors %v; want from 10 through the end", f.archiveCalls, f.cursors)
	}
}

// Free pages stay in the file when the database is not in incremental mode,
// and a vacuum step that yielded is retried soon.
func TestHistoryRetentionVacuumOnlyInIncrementalMode(t *testing.T) {
	f := newHistoryStore()
	f.free, f.incremental = 3000, false
	h := historyRetentionFor(f)
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.vacuumSteps) != 0 || h.more {
		t.Fatalf("vacuumed a non-incremental database: %v", f.vacuumSteps)
	}

	f.incremental, f.yieldVacuum = true, true
	if err := h.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.wait() != yieldRetry || h.vacuum.size != minYieldingBatch {
		t.Fatalf("after a yielded step: wait %s, step %d; want %s and a step of %d pages", h.wait(), h.vacuum.size, yieldRetry, minYieldingBatch)
	}
}

// The first pass waits ten minutes, clear of AO's startup and the other
// janitors' first passes, and a cancelled worker stops at once.
func TestHistoryRetentionWaitsOutItsStartDelayAndStopsOnCancel(t *testing.T) {
	if got := NewHistoryRetention(newHistoryStore(), HistoryRetentionConfig{}).startDelay; got != DefaultHistoryStartDelay {
		t.Fatalf("start delay = %v, want %v", got, DefaultHistoryStartDelay)
	}
	waiting := newHistoryStore()
	waiting.archiveEnd = 5
	ctx, cancel := context.WithCancel(context.Background())
	done := NewHistoryRetention(waiting, HistoryRetentionConfig{StartDelay: time.Hour, Pause: -1}).Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a worker cancelled during its start delay must stop")
	}
	if len(waiting.order) != 0 {
		t.Fatalf("calls = %v, want none before the start delay ends", waiting.order)
	}

	immediate := newHistoryStore()
	immediate.archiveEnd = 5
	ctx, cancel = context.WithCancel(context.Background())
	done = NewHistoryRetention(immediate, HistoryRetentionConfig{StartDelay: -1, Pause: -1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Start(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if len(immediate.archiveCalls) != 1 {
		t.Fatalf("archive calls = %d, want the first pass at once with no start delay", len(immediate.archiveCalls))
	}
}

// A store error is reported and ends the pass.
func TestHistoryRetentionReportsStoreErrors(t *testing.T) {
	boom := errors.New("disk I/O error")
	f := &failingHistoryStore{historyStore: newHistoryStore(), err: boom}
	f.archiveEnd = 5
	h := NewHistoryRetention(f, HistoryRetentionConfig{Pause: -1, StartDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := h.RunOnce(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("RunOnce = %v; want the store error", err)
	}
	if f.cursors[historyArchiveCursor] != 0 {
		t.Fatal("a failed batch moved the cursor")
	}
}

type failingHistoryStore struct {
	*historyStore
	err error
}

func (f *failingHistoryStore) SweepProviderEventArchive(context.Context, time.Time, int64, int64, int64) (int64, int64, int64, int64, error) {
	return 0, 0, 0, 0, f.err
}
