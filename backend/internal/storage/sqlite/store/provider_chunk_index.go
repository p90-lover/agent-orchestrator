package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// providerChunkIndexName is the index both provider-chunk janitor reads name
// (pruneChunkCandidatesSQL, firstChunkReceivedSinceSQL).
const providerChunkIndexName = "idx_conversation_provider_events_chunks"

// createProviderChunkIndexSQL builds that index. Its condition is
// chunkRowCondition without the alias: the same literals in the same order,
// because SQLite matches a partial index to a query by the text of the
// condition. It holds the chunk rows alone, which the janitor keeps few, and
// every column both reads need, so neither read opens a row.
const createProviderChunkIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_conversation_provider_events_chunks
    ON conversation_provider_events (id, received_at, conversation_id, method)
    WHERE method IN ('reasoning.delta', 'message.delta', 'command.output.delta')`

// warmProviderChunkRowsSQL reads from one id range of the archive what the
// index build reads from it: method from every row, received_at and
// conversation_id from the chunk rows. NOT INDEXED keeps it on the table, where
// the build reads; the aggregates make SQLite load the two columns. Its
// parameters are the range's afterID and throughID.
const warmProviderChunkRowsSQL = `
SELECT COUNT(*), MAX(e.received_at), MAX(e.conversation_id)
FROM conversation_provider_events AS e NOT INDEXED
WHERE e.id > ? AND e.id <= ?
  AND ` + chunkRowCondition

const (
	// providerChunkIndexFirstHold is how long the first build keeps the writer
	// once a foreground write is waiting for it. Measured 2026-11-16 at low I/O
	// priority: over 200,000 chunk rows among 20,000 kept events of 6 KB (a
	// 324 MB ao.db) the read took 2.0 s and the build then held the writer
	// 2.5 s, about 8 s for each GB of archive. This covers one of several GB
	// from a warm file cache, and still ends a build that reads a cold disk.
	providerChunkIndexFirstHold = 30 * time.Second
	// providerChunkIndexMaxHold bounds the hold a build that gave way grows to.
	providerChunkIndexMaxHold = 8 * time.Minute
	// providerChunkIndexIdleHold ends a build nobody is waiting behind.
	providerChunkIndexIdleHold = 10 * time.Minute
	// providerChunkWarmStep is the id range one warm-up statement reads. Each
	// statement is its own short read, so the long warm-up never keeps the
	// write-ahead log from being copied into the database (CheckpointWAL).
	providerChunkWarmStep int64 = 100_000
)

// EnsureProviderChunkIndex reports whether the provider-chunk janitor's index
// exists, and builds it when it does not. The janitor calls it before every
// pass and skips the pass until it answers true.
//
// The index used to be built by migration 9010, and a migration runs before AO
// listens. The build reads the whole archive table, the largest in ao.db: with
// a cold file cache on a busy hard disk that is minutes during which AO answers
// nothing. The launcher kills an AO that is not ready within 15 minutes, a
// build cut off there rolls back, and the next start begins it again. On
// 2026-11-16 the first start after the update that brought migration 9009's
// index (a 4.7 GB ao.db) was not ready in those 15 minutes and was killed; that
// index build is the likeliest cause.
//
// So the build runs here, after startup, in two steps:
//
//   - The archive is read once on a read connection, in id ranges, with no
//     lock: the slow part, and AO keeps answering through it. It leaves the
//     table's pages in the file cache.
//   - CREATE INDEX then runs under the single writer, mostly from that cache.
//     An index build cannot be split, so instead of shrinking it gives way like
//     any background write (maintain) but with a longer hold: once it has held
//     the writer for the hold time and a write is waiting, it is interrupted
//     and rolled back, this call answers false, and the next call tries again
//     with twice the hold. A build nobody waits behind may run longer.
//
// An install that ran migration 9010 while it still built the index has it
// already; that is found first and nothing is read.
func (s *Store) EnsureProviderChunkIndex(ctx context.Context, logger *slog.Logger) (bool, error) {
	if s.chunkIndexReady.Load() {
		return true, nil
	}
	s.chunkIndexMu.Lock()
	defer s.chunkIndexMu.Unlock()
	exists, err := s.providerChunkIndexExists(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		s.chunkIndexReady.Store(true)
		return true, nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	readStarted := time.Now()
	chunkRows, err := s.warmProviderChunkRows(ctx)
	if err != nil {
		return false, fmt.Errorf("provider chunk index: read the archive: %w", err)
	}
	read := time.Since(readStarted).Round(time.Millisecond)

	hold := s.chunkIndexHold
	buildStarted := time.Now()
	_, err = s.maintainWithin(ctx, hold, max(hold, s.chunkIndexIdleHold), func(ctx context.Context) (int64, error) {
		_, err := s.writeDB.ExecContext(ctx, createProviderChunkIndexSQL)
		return 0, err
	})
	held := time.Since(buildStarted).Round(time.Millisecond)
	if errors.Is(err, ports.ErrMaintenanceYielded) {
		s.chunkIndexHold = min(2*hold, max(hold, providerChunkIndexMaxHold))
		logger.Warn("provider chunk index build gave the writer back; the next try may hold it longer",
			"chunkRows", chunkRows, "read", read, "held", held, "hold", hold, "nextHold", s.chunkIndexHold)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provider chunk index: build: %w", err)
	}
	s.chunkIndexReady.Store(true)
	logger.Info("provider chunk index built", "chunkRows", chunkRows, "read", read, "held", held)
	return true, nil
}

func (s *Store) providerChunkIndexExists(ctx context.Context) (bool, error) {
	var found int
	err := s.readDB.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?`, providerChunkIndexName).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("provider chunk index: look it up: %w", err)
	}
	return true, nil
}

// warmProviderChunkRows reads the archive the way the index build will, one id
// range at a time, and returns how many chunk rows it passed.
func (s *Store) warmProviderChunkRows(ctx context.Context) (int64, error) {
	var first, newest sql.NullInt64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM conversation_provider_events`).Scan(&first, &newest); err != nil {
		return 0, err
	}
	if !first.Valid || !newest.Valid {
		return 0, nil
	}
	var total int64
	for after := first.Int64 - 1; after < newest.Int64; after += providerChunkWarmStep {
		var rows int64
		var receivedAt, conversationID sql.NullString
		if err := s.readDB.QueryRowContext(ctx, warmProviderChunkRowsSQL, after, after+providerChunkWarmStep).
			Scan(&rows, &receivedAt, &conversationID); err != nil {
			return 0, err
		}
		total += rows
	}
	return total, nil
}
