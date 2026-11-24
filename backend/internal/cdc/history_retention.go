package cdc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// DefaultHistoryArchiveAge is how long the raw provider-event archive keeps
	// a row: a week of evidence of what a provider sent, for debugging a chat
	// that went wrong. Nothing in the app reads the archive.
	DefaultHistoryArchiveAge = 7 * 24 * time.Hour
	// DefaultHistoryCardAge is how old a tool card gets before its long texts
	// are trimmed (command output, tool results, file contents and diffs).
	DefaultHistoryCardAge = 30 * 24 * time.Hour
	// DefaultHistoryCardMaxText is how much of each long text an old card keeps.
	DefaultHistoryCardMaxText = 4 * 1024
	// DefaultHistoryArchiveBatch matches the chunk janitor's delete size.
	DefaultHistoryArchiveBatch int64 = 2_000
	// DefaultHistoryCardBatch is smaller: each trimmed card is rewritten whole.
	DefaultHistoryCardBatch int64 = 200
	// DefaultHistoryVacuumStep is how many pages one vacuum step frees (4 MB).
	DefaultHistoryVacuumStep int64 = 1_024
	// DefaultHistoryVacuumPassBytes bounds what one pass gives back, so moving
	// live pages (random I/O on a hard disk) comes in short stretches.
	DefaultHistoryVacuumPassBytes int64 = 64 << 20
	// DefaultHistoryVacuumKeepBytes stays free in the file for new rows.
	DefaultHistoryVacuumKeepBytes int64 = 16 << 20
	// DefaultHistoryStartDelay keeps the first pass clear of AO's startup and of
	// the other janitors' first passes (DefaultRetentionStartDelay).
	DefaultHistoryStartDelay = 10 * time.Minute
	// DefaultHistoryBacklogRetry is how soon a pass that left work (a backlog
	// sweep, free pages over the pass budget) comes back.
	DefaultHistoryBacklogRetry = 2 * time.Minute
	defaultHistoryBatches      = 50

	historyArchiveCursor = "provider_archive"
	historyCardCursor    = "activity_cards"
)

// HistoryRetentionStore is the storage surface of the chat history retention.
type HistoryRetentionStore interface {
	RetentionCursor(ctx context.Context, name string) (int64, error)
	SetRetentionCursor(ctx context.Context, name string, position int64, now time.Time) error
	ProviderEventIDReceivedBefore(ctx context.Context, afterID int64, cutoff time.Time) (int64, error)
	SweepProviderEventArchive(ctx context.Context, receivedBefore time.Time, afterID, throughID, limit int64) (found, deleted, emptied, lastID int64, err error)
	ActivityRowIDCreatedBefore(ctx context.Context, afterRowID int64, cutoff time.Time) (int64, error)
	TrimActivityBodies(ctx context.Context, createdBefore time.Time, afterRowID, throughRowID, limit int64, maxText int) (found, trimmed, lastRowID int64, err error)
	FreePages(ctx context.Context) (free, pageSize int64, incremental bool, err error)
	IncrementalVacuumStep(ctx context.Context, pages int64) (int64, error)
}

// HistoryRetentionConfig customizes the chat history retention; zero values
// use the production defaults.
type HistoryRetentionConfig struct {
	ArchiveAge      time.Duration
	CardAge         time.Duration
	CardMaxText     int
	ArchiveBatch    int64
	CardBatch       int64
	VacuumStep      int64
	VacuumPassBytes int64
	VacuumKeepBytes int64
	Interval        time.Duration
	BacklogRetry    time.Duration
	// Pause between batches and vacuum steps; negative means none (tests).
	Pause time.Duration
	// StartDelay before the first pass; negative means none.
	StartDelay time.Duration
	Clock      func() time.Time
	Logger     *slog.Logger
}

// HistoryRetention bounds what old chats keep in ao.db and gives the space
// back to the disk. On 2026-11-24 ao.db was 6.6 GB and grew about 0.22 GB a
// day: the raw provider-event archive (4.3 GB, read by nothing in the app) and
// tool-card bodies (1.9 GB) were almost all of it, the chat text 0.1 GB.
//
// Each pass, one step at a time so they never compete for the writer:
//  1. the archive sweep retires rows older than ArchiveAge
//     (Store.SweepProviderEventArchive);
//  2. the card trim shortens long texts of cards older than CardAge
//     (Store.TrimActivityBodies);
//  3. vacuum steps give free pages back to the file system
//     (Store.IncrementalVacuumStep), up to VacuumPassBytes a pass.
//
// The sweeps walk their tables in id order from a cursor the store keeps, so
// neither a pass nor an AO restart reads again what was already handled. Every
// write yields the writer to a waiting chat write; a pass that yielded or left
// work comes back sooner than Interval.
type HistoryRetention struct {
	store           HistoryRetentionStore
	archiveAge      time.Duration
	cardAge         time.Duration
	cardMaxText     int
	vacuumStep      int64
	vacuumPassBytes int64
	vacuumKeepBytes int64
	interval        time.Duration
	backlogRetry    time.Duration
	pause           time.Duration
	startDelay      time.Duration
	clock           func() time.Time
	logger          *slog.Logger
	maxBatches      int

	archive historySweep
	cards   historySweep
	vacuum  yieldingBatch
	// more is set by a pass that left work for the next one.
	more bool
	// reportedNoVacuum keeps the "not incremental" note to one line.
	reportedNoVacuum bool
}

// historySweep is one table's walk. position is where the store's cursor
// says the walk has got; rows at or below it are handled.
type historySweep struct {
	name     string
	loaded   bool
	position int64
	size     yieldingBatch
}

// NewHistoryRetention constructs the chat history retention worker.
func NewHistoryRetention(store HistoryRetentionStore, cfg HistoryRetentionConfig) *HistoryRetention {
	h := &HistoryRetention{
		store: store, archiveAge: cfg.ArchiveAge, cardAge: cfg.CardAge, cardMaxText: cfg.CardMaxText,
		vacuumStep: cfg.VacuumStep, vacuumPassBytes: cfg.VacuumPassBytes, vacuumKeepBytes: cfg.VacuumKeepBytes,
		interval: cfg.Interval, backlogRetry: cfg.BacklogRetry, pause: cfg.Pause, startDelay: cfg.StartDelay,
		clock: cfg.Clock, logger: cfg.Logger, maxBatches: defaultHistoryBatches,
	}
	if h.archiveAge <= 0 {
		h.archiveAge = DefaultHistoryArchiveAge
	}
	if h.cardAge <= 0 {
		h.cardAge = DefaultHistoryCardAge
	}
	if h.cardMaxText <= 0 {
		h.cardMaxText = DefaultHistoryCardMaxText
	}
	archiveBatch, cardBatch := cfg.ArchiveBatch, cfg.CardBatch
	if archiveBatch <= 0 {
		archiveBatch = DefaultHistoryArchiveBatch
	}
	if cardBatch <= 0 {
		cardBatch = DefaultHistoryCardBatch
	}
	if h.vacuumStep <= 0 {
		h.vacuumStep = DefaultHistoryVacuumStep
	}
	if h.vacuumPassBytes <= 0 {
		h.vacuumPassBytes = DefaultHistoryVacuumPassBytes
	}
	if h.vacuumKeepBytes <= 0 {
		h.vacuumKeepBytes = DefaultHistoryVacuumKeepBytes
	}
	if h.interval <= 0 {
		h.interval = DefaultRetentionInterval
	}
	if h.backlogRetry <= 0 {
		h.backlogRetry = DefaultHistoryBacklogRetry
	}
	if h.pause == 0 {
		h.pause = DefaultProviderDeltaPause
	}
	if h.startDelay == 0 {
		h.startDelay = DefaultHistoryStartDelay
	}
	if h.clock == nil {
		h.clock = time.Now
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	h.archive = historySweep{name: historyArchiveCursor, size: newYieldingBatch(archiveBatch)}
	h.cards = historySweep{name: historyCardCursor, size: newYieldingBatch(cardBatch)}
	h.vacuum = newYieldingBatch(h.vacuumStep)
	return h
}

// Start runs a pass after the start delay and then whenever the last one says
// to, until ctx is cancelled; the returned channel closes when it has stopped.
func (h *HistoryRetention) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !waitStartDelay(ctx, h.startDelay) {
			return
		}
		h.run(ctx)
		for {
			t := time.NewTimer(h.wait())
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				h.run(ctx)
			}
		}
	}()
	return done
}

// wait is how long until the next pass: sooner when the last one gave the
// writer back to a chat write or left work behind.
func (h *HistoryRetention) wait() time.Duration {
	d := min(h.interval, h.archive.size.wait(h.interval), h.cards.size.wait(h.interval), h.vacuum.wait(h.interval))
	if h.more {
		d = min(d, h.backlogRetry)
	}
	return d
}

func (h *HistoryRetention) run(ctx context.Context) {
	if err := h.RunOnce(ctx); err != nil && ctx.Err() == nil {
		h.logger.Warn("chat history retention failed", "err", err)
	}
}

// RunOnce performs one bounded pass: the archive sweep, then the card trim,
// then vacuum steps. A step that gave the writer back to a chat write ends the
// pass; the next one comes after yieldRetry and continues where it stopped.
func (h *HistoryRetention) RunOnce(ctx context.Context) error {
	h.more = false
	now := h.clock().UTC()

	archiveBefore := now.Add(-h.archiveAge)
	var deleted, emptied int64
	finished, err := h.sweep(ctx, &h.archive, now,
		func(after int64) (int64, error) {
			return h.store.ProviderEventIDReceivedBefore(ctx, after, archiveBefore)
		},
		func(after, through, limit int64) (int64, int64, error) {
			found, d, e, last, err := h.store.SweepProviderEventArchive(ctx, archiveBefore, after, through, limit)
			deleted += d
			emptied += e
			return found, last, err
		})
	if deleted > 0 || emptied > 0 {
		h.logger.Info("chat history retention retired old provider events", "deleted", deleted, "emptied", emptied)
	}
	if err != nil || !finished {
		return err
	}

	cardsBefore := now.Add(-h.cardAge)
	var trimmed int64
	finished, err = h.sweep(ctx, &h.cards, now,
		func(after int64) (int64, error) {
			return h.store.ActivityRowIDCreatedBefore(ctx, after, cardsBefore)
		},
		func(after, through, limit int64) (int64, int64, error) {
			found, n, last, err := h.store.TrimActivityBodies(ctx, cardsBefore, after, through, limit, h.cardMaxText)
			trimmed += n
			return found, last, err
		})
	if trimmed > 0 {
		h.logger.Info("chat history retention trimmed old tool cards", "cards", trimmed)
	}
	if err != nil || !finished {
		return err
	}
	return h.vacuumPass(ctx)
}

// sweep walks one table from its cursor through the last old row, a batch at a
// time, and records how far it got. It reports false when a batch gave the
// writer back to a chat write.
func (h *HistoryRetention) sweep(
	ctx context.Context,
	s *historySweep,
	now time.Time,
	through func(after int64) (int64, error),
	batch func(after, through, limit int64) (found, lastID int64, err error),
) (bool, error) {
	if !s.loaded {
		position, err := h.store.RetentionCursor(ctx, s.name)
		if err != nil {
			return false, err
		}
		s.position, s.loaded = position, true
	}
	end, err := through(s.position)
	if err != nil {
		return false, err
	}
	next := s.position
	defer func() { h.advance(ctx, s, next, now) }()
	for i := 0; i < h.maxBatches && next < end; i++ {
		size := s.size.size
		found, last, err := batch(next, end, size)
		// A chat write needed the writer: stop here and come back soon with a
		// smaller batch. Nothing failed, so it is not an error.
		if errors.Is(err, ports.ErrMaintenanceYielded) {
			s.size.yielded()
			h.logger.Info("chat history retention gave the writer to a waiting write", "sweep", s.name, "nextBatch", s.size.size)
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if found < size {
			next = end
			break
		}
		next = last
		if !h.sleep(ctx) {
			return false, ctx.Err()
		}
	}
	if next < end {
		h.more = true
	}
	s.size.finished()
	return true, nil
}

// advance moves a sweep's cursor forward and records it. A failed record only
// costs the next AO start a reread of these rows, so it is logged, not raised.
func (h *HistoryRetention) advance(ctx context.Context, s *historySweep, position int64, now time.Time) {
	if position <= s.position {
		return
	}
	s.position = position
	if err := h.store.SetRetentionCursor(ctx, s.name, position, now); err != nil && ctx.Err() == nil {
		h.logger.Info("chat history retention could not record its place yet", "sweep", s.name, "err", err)
	}
}

// vacuumPass gives free pages back to the file system in small steps, keeping
// VacuumKeepBytes free for new rows and stopping at VacuumPassBytes.
func (h *HistoryRetention) vacuumPass(ctx context.Context) error {
	free, pageSize, incremental, err := h.store.FreePages(ctx)
	if err != nil {
		return err
	}
	if !incremental || pageSize <= 0 {
		if !h.reportedNoVacuum {
			h.reportedNoVacuum = true
			h.logger.Info("chat history retention leaves free pages in the database: it is not in incremental auto-vacuum mode")
		}
		return nil
	}
	keep := h.vacuumKeepBytes / pageSize
	var freed int64
	defer func() {
		if freed > 0 {
			h.logger.Info("chat history retention gave space back to the disk", "bytes", freed*pageSize)
		}
	}()
	for free > keep && freed*pageSize < h.vacuumPassBytes {
		n, err := h.store.IncrementalVacuumStep(ctx, min(h.vacuum.size, free-keep))
		if errors.Is(err, ports.ErrMaintenanceYielded) {
			h.vacuum.yielded()
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		freed += n
		free -= n
		if !h.sleep(ctx) {
			return ctx.Err()
		}
	}
	if free > keep {
		h.more = true
	}
	h.vacuum.finished()
	return nil
}

// sleep waits out the pause between writes and reports whether ctx is live.
func (h *HistoryRetention) sleep(ctx context.Context) bool {
	if h.pause <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(h.pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
