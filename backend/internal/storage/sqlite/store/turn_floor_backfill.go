package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// floorlessTurnsIndexName holds the turns that still have no floor_sequence:
// the ones written before migration 9011, which records a floor on every
// insert. The backfill empties it, so on a later start one page read says
// there is nothing left to do; without it, finding that out reads every turn
// row (3,008 pages for 6,000 test turns). It carries the turn id, so listing a
// chat's floorless turns reads no turn row.
const floorlessTurnsIndexName = "idx_conversation_turns_floorless"

const createFloorlessTurnsIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_conversation_turns_floorless
    ON conversation_turns (conversation_id, id)
    WHERE floor_sequence IS NULL`

// countFloorlessTurnsSQL reads one rowid range of the table the way the index
// build will, on a read connection and with no lock, so the build that follows
// under the writer runs from the file cache. A NULL floor is decided from the
// row header, so no diff_json overflow page is read for it. Its parameters are
// the range's afterRowid and throughRowid.
const countFloorlessTurnsSQL = `
SELECT COUNT(*) FROM conversation_turns NOT INDEXED
WHERE conversation_turns.rowid > ? AND conversation_turns.rowid <= ?
  AND conversation_turns.floor_sequence IS NULL`

// turnFloorWarmStep is the rowid range one warm-up statement reads: each is
// its own short read, so the warm-up never keeps the write-ahead log from
// being copied into the database (CheckpointWAL).
const turnFloorWarmStep int64 = 5_000

// floorlessTurnChatsSQL lists the chats that still have floorless turns, from
// the index alone. Forked chats come first: on every timeline page their
// lineage EXISTS walks the chat from its first row for each floorless turn of
// a parent branch. Then the most recently active chats, which are the ones
// likely to be opened. LEFT JOIN, so a chat whose conversation row is gone is
// still listed and the index can empty.
const floorlessTurnChatsSQL = `
SELECT floorless.conversation_id
FROM (
    SELECT conversation_turns.conversation_id
    FROM conversation_turns INDEXED BY idx_conversation_turns_floorless
    WHERE conversation_turns.floor_sequence IS NULL
    GROUP BY conversation_turns.conversation_id
) AS floorless
LEFT JOIN conversations ON conversations.id = floorless.conversation_id
ORDER BY EXISTS (
    SELECT 1 FROM conversation_branches
    WHERE conversation_branches.conversation_id = floorless.conversation_id
      AND conversation_branches.parent_branch_id IS NOT NULL
) DESC, conversations.updated_at DESC, floorless.conversation_id`

// floorlessTurnsOfChatSQL lists one chat's floorless turns.
const floorlessTurnsOfChatSQL = `
SELECT conversation_turns.id
FROM conversation_turns INDEXED BY idx_conversation_turns_floorless
WHERE conversation_turns.conversation_id = ?
  AND conversation_turns.floor_sequence IS NULL`

// turnFloorMessagesSQL and turnFloorActivitiesSQL read one range of a chat's
// sequences in order. Messages come from the (conversation_id, sequence)
// index and their table rows: turn_id is the row's third column, inside the
// part of the row on its leaf page, so no overflow page (long reply text) is
// read. Activities come from idx_conversation_activities_page_keys alone.
const (
	turnFloorMessagesSQL = `
SELECT conversation_messages.sequence, conversation_messages.turn_id
FROM conversation_messages
WHERE conversation_messages.conversation_id = ?
  AND conversation_messages.sequence > ?
  AND conversation_messages.sequence <= ?
  AND conversation_messages.turn_id IS NOT NULL
ORDER BY conversation_messages.sequence`
	turnFloorActivitiesSQL = `
SELECT conversation_activities.sequence, conversation_activities.turn_id
FROM conversation_activities
WHERE conversation_activities.conversation_id = ?
  AND conversation_activities.sequence > ?
  AND conversation_activities.sequence <= ?
  AND conversation_activities.turn_id IS NOT NULL
ORDER BY conversation_activities.sequence`
)

// setTurnFloorSQL never replaces a floor: one recorded at insert, or by an
// earlier pass, stays.
const setTurnFloorSQL = `
UPDATE conversation_turns SET floor_sequence = ?
WHERE id = ? AND conversation_id = ? AND floor_sequence IS NULL`

const (
	// The index build holds the writer like the provider-chunk index build
	// (EnsureProviderChunkIndex): it cannot be split, so a build that gives way
	// tries again with twice the hold.
	floorlessTurnsIndexFirstHold = 30 * time.Second
	floorlessTurnsIndexMaxHold   = 8 * time.Minute
	floorlessTurnsIndexIdleHold  = 10 * time.Minute
)

// turnFloorPace spaces the backfill's reads and writes, so a shared hard disk
// still serves chat reads and the single writer still serves chat writes.
type turnFloorPace struct {
	// readStep is how many sequences one read statement covers. Each is its own
	// short read, so the pass never keeps the write-ahead log from being copied
	// into the database (CheckpointWAL).
	readStep int64
	// readPause follows each read statement, writePause each write batch, and
	// chatPause each chat.
	readPause, writePause, chatPause time.Duration
	// writeBatch is how many turns one write transaction gives a floor. Each
	// rewrites the turn's row, its diff included.
	writeBatch int
	// yieldWait follows a write batch that gave the writer to a chat write,
	// before a smaller batch is tried.
	yieldWait time.Duration
	// matchElapsed makes the pause after a statement at least as long as the
	// statement took, so a slow disk slows the backfill down with it.
	matchElapsed bool
}

var defaultTurnFloorPace = turnFloorPace{
	readStep:     2_000,
	readPause:    50 * time.Millisecond,
	writePause:   250 * time.Millisecond,
	chatPause:    time.Second,
	writeBatch:   200,
	yieldWait:    5 * time.Second,
	matchElapsed: true,
}

// pacedBy is the pause a statement that took elapsed earns.
func (p turnFloorPace) pacedBy(elapsed time.Duration) time.Duration {
	if !p.matchElapsed {
		return 0
	}
	return elapsed
}

// minTurnFloorWriteBatch is the smallest batch a yielding backfill falls back to.
const minTurnFloorWriteBatch = 10

// turnFloorBackfillReport says what one BackfillTurnFloors call did.
type turnFloorBackfillReport struct {
	chats  int
	turns  int64
	yields int
}

// BackfillTurnFloors gives every turn written before migration 9011 its
// floor_sequence, chat by chat, and reports whether every turn now has one.
//
// Per-turn lookups start the chat's (conversation_id, sequence) index at the
// turn's floor (migration 9011). A turn with no floor is read from sequence 0:
// in a forked chat, every timeline page then walks the chat from its first row
// for each such turn of a parent branch. Migration 9011 could not fill the old
// turns itself, because the floors come from reading every chat's rows, and a
// migration runs before AO listens.
//
// So this runs after startup (cdc.TurnFloorBackfill). A turn's floor is the
// lowest sequence among its messages and activities, which is where the
// lookups must start: all of them compare sequence >= floor. Each chat is read
// once, in sequence order, in short read statements on a read connection, so
// the first row seen of a turn is its lowest. A turn with no row gets the
// chat's latest_sequence, read before the walk: any row it gets later takes a
// higher sequence from the same counter (NextConversationSequence), the same
// reason migration 9011's inserts record latest_sequence. A row the walk did
// not see was written after it, above latest_sequence, so no floor is above a
// row of its turn. The floors are written in small batches that give the
// writer to a waiting chat write (maintain).
//
// The first call builds idx_conversation_turns_floorless over the turns that
// have no floor. Once every turn has one the index is empty, and a later call,
// on any start, reads one page of it and returns true.
func (s *Store) BackfillTurnFloors(ctx context.Context, logger *slog.Logger) (bool, error) {
	if s.turnFloorsDone.Load() {
		return true, nil
	}
	s.turnFloorMu.Lock()
	defer s.turnFloorMu.Unlock()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ready, err := s.ensureFloorlessTurnsIndex(ctx, logger)
	if err != nil || !ready {
		return false, err
	}
	chats, err := s.floorlessTurnChats(ctx)
	if err != nil {
		return false, fmt.Errorf("turn floors: list chats: %w", err)
	}
	var report turnFloorBackfillReport
	started := time.Now()
	for i, chat := range chats {
		if i > 0 {
			if err := sleepContext(ctx, s.turnFloorPace.chatPause); err != nil {
				logTurnFloorBackfill(logger, report, started, true)
				return false, err
			}
		}
		turns, yields, err := s.backfillChatTurnFloors(ctx, chat)
		report.turns += turns
		report.yields += yields
		if err != nil {
			logTurnFloorBackfill(logger, report, started, true)
			return false, fmt.Errorf("turn floors: chat %s: %w", chat, err)
		}
		report.chats++
	}
	left, err := s.anyFloorlessTurn(ctx)
	if err != nil {
		return false, err
	}
	logTurnFloorBackfill(logger, report, started, left)
	if left {
		return false, nil
	}
	s.turnFloorsDone.Store(true)
	return true, nil
}

func logTurnFloorBackfill(logger *slog.Logger, report turnFloorBackfillReport, started time.Time, left bool) {
	if report.chats == 0 && report.turns == 0 {
		return
	}
	logger.Info("turn floors backfilled", "chats", report.chats, "turns", report.turns,
		"yields", report.yields, "took", time.Since(started).Round(time.Millisecond), "left", left)
}

// ensureFloorlessTurnsIndex builds idx_conversation_turns_floorless when it is
// missing, the way EnsureProviderChunkIndex builds its index: read on a read
// connection first, then build under the writer with a hold that doubles each
// time the build gives way.
func (s *Store) ensureFloorlessTurnsIndex(ctx context.Context, logger *slog.Logger) (bool, error) {
	exists, err := s.indexExists(ctx, floorlessTurnsIndexName)
	if err != nil || exists {
		return exists, err
	}
	readStarted := time.Now()
	floorless, err := s.warmFloorlessTurns(ctx)
	if err != nil {
		return false, fmt.Errorf("turn floors: read the turns: %w", err)
	}
	read := time.Since(readStarted).Round(time.Millisecond)
	hold := s.turnFloorIndexHold
	buildStarted := time.Now()
	_, err = s.maintainWithin(ctx, hold, max(hold, floorlessTurnsIndexIdleHold), func(ctx context.Context) (int64, error) {
		_, err := s.writeDB.ExecContext(ctx, createFloorlessTurnsIndexSQL)
		return 0, err
	})
	held := time.Since(buildStarted).Round(time.Millisecond)
	if errors.Is(err, ports.ErrMaintenanceYielded) {
		s.turnFloorIndexHold = min(2*hold, max(hold, floorlessTurnsIndexMaxHold))
		logger.Warn("floorless turn index build gave the writer back; the next try may hold it longer",
			"floorless", floorless, "read", read, "held", held, "hold", hold, "nextHold", s.turnFloorIndexHold)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("turn floors: build the floorless turn index: %w", err)
	}
	logger.Info("floorless turn index built", "floorless", floorless, "read", read, "held", held)
	return true, nil
}

// warmFloorlessTurns reads the turns table one rowid range at a time, pausing
// after each read as the backfill's reads do, and returns how many turns have
// no floor.
func (s *Store) warmFloorlessTurns(ctx context.Context) (int64, error) {
	var first, last sql.NullInt64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT MIN(rowid), MAX(rowid) FROM conversation_turns`).Scan(&first, &last); err != nil {
		return 0, err
	}
	if !first.Valid || !last.Valid {
		return 0, nil
	}
	var total int64
	for after := first.Int64 - 1; after < last.Int64; after += turnFloorWarmStep {
		started := time.Now()
		var n int64
		if err := s.readDB.QueryRowContext(ctx, countFloorlessTurnsSQL, after, after+turnFloorWarmStep).Scan(&n); err != nil {
			return 0, err
		}
		total += n
		if err := sleepContext(ctx, s.turnFloorPace.pacedBy(time.Since(started))); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (s *Store) indexExists(ctx context.Context, name string) (bool, error) {
	var found int
	err := s.readDB.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up index %s: %w", name, err)
	}
	return true, nil
}

func (s *Store) floorlessTurnChats(ctx context.Context) ([]string, error) {
	return s.readStrings(ctx, floorlessTurnChatsSQL)
}

func (s *Store) anyFloorlessTurn(ctx context.Context) (bool, error) {
	var found int
	err := s.readDB.QueryRowContext(ctx, `
SELECT 1 FROM conversation_turns INDEXED BY idx_conversation_turns_floorless
WHERE conversation_turns.floor_sequence IS NULL LIMIT 1`).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("turn floors: look for a floorless turn: %w", err)
	}
	return true, nil
}

func (s *Store) readStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

type turnFloor struct {
	turnID string
	floor  int64
}

// backfillChatTurnFloors gives one chat's floorless turns their floor and
// returns how many it wrote and how often a batch gave the writer back.
func (s *Store) backfillChatTurnFloors(ctx context.Context, conversationID string) (int64, int, error) {
	pending, err := s.readStrings(ctx, floorlessTurnsOfChatSQL, conversationID)
	if err != nil {
		return 0, 0, fmt.Errorf("list floorless turns: %w", err)
	}
	if len(pending) == 0 {
		return 0, 0, nil
	}
	// Read before the walk: every row at or below it exists by now, and every
	// later row is above it.
	var latest int64
	err = s.readDB.QueryRowContext(ctx,
		`SELECT latest_sequence FROM conversations WHERE id = ?`, conversationID).Scan(&latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("read latest sequence: %w", err)
	}
	// With no conversation row nothing can add rows; latest stays 0 and the walk
	// reads nothing, so its turns get floor 0, which is where they are read from
	// today.
	first, err := s.firstSequenceOfEachTurn(ctx, conversationID, latest, pending)
	if err != nil {
		return 0, 0, err
	}
	floors := make([]turnFloor, 0, len(pending))
	for _, turnID := range pending {
		floor, seen := first[turnID]
		if !seen {
			floor = latest
		}
		floors = append(floors, turnFloor{turnID: turnID, floor: floor})
	}
	sort.Slice(floors, func(i, j int) bool { return floors[i].floor < floors[j].floor })
	return s.writeTurnFloors(ctx, conversationID, floors)
}

// firstSequenceOfEachTurn walks one chat's messages and activities up to
// through, one range of sequences at a time in order, and returns the first
// sequence each pending turn has. Once a range has been read from both tables,
// a turn seen in it has no lower row left to find, so the walk stops as soon
// as every pending turn has been seen: those are old turns, whose first rows
// come early in the chat. It reads to the end only for a turn with no row.
//
// Each statement is followed by a pause at least as long as it took, so on a
// cold disk the walk takes at most about half of the disk's time from chat
// reads.
func (s *Store) firstSequenceOfEachTurn(ctx context.Context, conversationID string, through int64, pending []string) (map[string]int64, error) {
	wanted := make(map[string]bool, len(pending))
	for _, turnID := range pending {
		wanted[turnID] = true
	}
	first := make(map[string]int64, len(pending))
	step := max(s.turnFloorPace.readStep, 1)
	for after := int64(0); after < through && len(first) < len(wanted); after += step {
		upTo := min(after+step, through)
		for _, query := range []string{turnFloorMessagesSQL, turnFloorActivitiesSQL} {
			started := time.Now()
			if err := s.readFirstSequences(ctx, query, conversationID, after, upTo, wanted, first); err != nil {
				return nil, fmt.Errorf("read sequences after %d: %w", after, err)
			}
			if err := sleepContext(ctx, max(s.turnFloorPace.readPause, s.turnFloorPace.pacedBy(time.Since(started)))); err != nil {
				return nil, err
			}
		}
	}
	return first, nil
}

func (s *Store) readFirstSequences(ctx context.Context, query, conversationID string, after, through int64, wanted map[string]bool, first map[string]int64) error {
	rows, err := s.readDB.QueryContext(ctx, query, conversationID, after, through)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sequence int64
		var turnID string
		if err := rows.Scan(&sequence, &turnID); err != nil {
			return err
		}
		if !wanted[turnID] {
			continue
		}
		if seen, ok := first[turnID]; !ok || sequence < seen {
			first[turnID] = sequence
		}
	}
	return rows.Err()
}

// writeTurnFloors writes floors in batches under maintain. A batch that gave
// the writer to a chat write was rolled back whole; after yieldWait a quarter
// of it is tried, and each batch that finishes doubles the size back.
func (s *Store) writeTurnFloors(ctx context.Context, conversationID string, floors []turnFloor) (int64, int, error) {
	full := max(s.turnFloorPace.writeBatch, 1)
	size := full
	var written int64
	yields := 0
	for len(floors) > 0 {
		batch := floors[:min(size, len(floors))]
		started := time.Now()
		n, err := s.maintain(ctx, func(ctx context.Context) (int64, error) {
			return s.writeTurnFloorBatch(ctx, conversationID, batch)
		})
		took := time.Since(started)
		if errors.Is(err, ports.ErrMaintenanceYielded) {
			yields++
			size = max(min(minTurnFloorWriteBatch, full), size/4)
			if err := sleepContext(ctx, s.turnFloorPace.yieldWait); err != nil {
				return written, yields, err
			}
			continue
		}
		if err != nil {
			return written, yields, fmt.Errorf("write floors: %w", err)
		}
		written += n
		floors = floors[len(batch):]
		size = min(full, size*2)
		if len(floors) > 0 {
			if err := sleepContext(ctx, max(s.turnFloorPace.writePause, s.turnFloorPace.pacedBy(took))); err != nil {
				return written, yields, err
			}
		}
	}
	return written, yields, nil
}

func (s *Store) writeTurnFloorBatch(ctx context.Context, conversationID string, batch []turnFloor) (int64, error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, setTurnFloorSQL)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()
	var written int64
	for _, floor := range batch {
		result, err := stmt.ExecContext(ctx, floor.floor, floor.turnID, conversationID)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		written += n
		if s.turnFloorWriteHook != nil {
			if err := s.turnFloorWriteHook(ctx); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// sleepContext waits d, or until ctx ends. A d of zero or less returns at once.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
