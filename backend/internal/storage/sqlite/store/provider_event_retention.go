package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// providerDeltaMethods are the per-chunk streaming events. They are the bulk
// of conversation_provider_events (one row per reasoning or message token
// chunk: 1.1M reasoning.delta rows, 718 MB, in a single day of one chat) and
// the projection keeps what they built: the message, the reasoning, the
// command's (truncated) output.
var providerDeltaMethods = []string{"reasoning.delta", "message.delta", "command.output.delta"}

// hostDeltaPosition splits a persistent chat host's delta event id,
// "acp-host:<identity>:<seq>:<index>", into the host it came from and its
// position there. A host's identity is random per process and its sequence only
// grows, and AO applies a host's events in that order, so a position at or
// behind the newest applied one is a replay. Any other event or id shape keeps
// the archive's per-event dedupe.
func hostDeltaPosition(method, providerEventID string) (source string, seq, idx int64, ok bool) {
	if !slices.Contains(providerDeltaMethods, method) || !strings.HasPrefix(providerEventID, "acp-host:") {
		return "", 0, 0, false
	}
	rest, idxText, found := cutLast(providerEventID, ":")
	if !found {
		return "", 0, 0, false
	}
	source, seqText, found := cutLast(rest, ":")
	if !found || strings.Count(source, ":") != 1 || source == "acp-host:" {
		return "", 0, 0, false
	}
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if err != nil || seq < 0 {
		return "", 0, 0, false
	}
	idx, err = strconv.ParseInt(idxText, 10, 64)
	if err != nil || idx < 0 {
		return "", 0, 0, false
	}
	return source, seq, idx, true
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// receivedAtCutoff renders a cutoff the way received_at is stored (the
// modernc driver writes time.Time as Go's String form, "2006-01-02
// 15:04:05.9999999 +0000 UTC"), so the text comparison orders correctly.
func receivedAtCutoff(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// settledDeltaPredicate matches streaming-chunk rows (alias e) that nothing can
// replay. Its parameters are the three providerDeltaMethods, then the anyBefore
// and settledBefore cutoffs.
const settledDeltaPredicate = `e.method IN (?, ?, ?)
  AND (
    e.received_at < ?
    OR (
      e.received_at < ?
      AND NOT EXISTS (
        SELECT 1 FROM conversation_turns AS t
        WHERE t.conversation_id = e.conversation_id
          AND t.state IN ('queued', 'running')
      )
    )
  )`

// PruneSettledProviderDeltas removes at most limit streaming-chunk rows with an
// id in (afterID, throughID] from the provider event archive: those older than
// settledBefore whose conversation has no queued or running turn, and any older
// than anyBefore. found is how many rows the read matched and lastID the
// highest of them, so the next batch starts after it; removed can be lower than
// found when a turn opened between the read and the delete.
//
// The archive has no production reader; it is evidence of what the provider
// sent, and its event identity deduplicates events a host replays before the
// turn's terminal event is acknowledged. Once no turn of the conversation is
// open, nothing can replay those chunks, so they only cost space and every
// write's index maintenance. Turn, item, approval and usage events are kept.
//
// The rows are found on a read connection and only their delete takes the
// writer. No index covers method or received_at, so the read walks the id range
// it is given, payloads included: given the whole archive, a batch that matched
// fewer rows than its limit read all of it. Right after a reboot, on a cold HDD
// cache, that read held the writer over a minute at startup (2026-10-24); once
// it moved to a read connection, the janitor still reread the 3.9 GB database
// from its first row on every batch and every pass, about 300 GB of reads in
// twelve hours (2026-11-13). Callers pass the range they have not covered yet.
func (s *Store) PruneSettledProviderDeltas(ctx context.Context, settledBefore, anyBefore time.Time, afterID, throughID, limit int64) (found, removed, lastID int64, err error) {
	if limit <= 0 || throughID <= afterID {
		return 0, 0, 0, nil
	}
	args := []any{providerDeltaMethods[0], providerDeltaMethods[1], providerDeltaMethods[2],
		receivedAtCutoff(anyBefore), receivedAtCutoff(settledBefore)}
	rows, err := s.readDB.QueryContext(ctx, `
SELECT e.id
FROM conversation_provider_events AS e
WHERE e.id > ? AND e.id <= ?
  AND `+settledDeltaPredicate+`
ORDER BY e.id ASC
LIMIT ?`, append(append([]any{afterID, throughID}, args...), limit)...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("prune settled provider deltas: find rows: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, 0, 0, fmt.Errorf("prune settled provider deltas: find rows: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, 0, fmt.Errorf("prune settled provider deltas: find rows: %w", err)
	}
	if len(ids) == 0 {
		return 0, 0, 0, nil
	}
	idList, err := json.Marshal(ids)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("prune settled provider deltas: %w", err)
	}
	// The delete gives the writer back to a waiting chat write (maintain).
	removed, err = s.maintain(ctx, func(ctx context.Context) (int64, error) {
		return s.deleteSettledProviderDeltas(ctx, string(idList), args)
	})
	if err != nil {
		return 0, 0, 0, err
	}
	return int64(len(ids)), removed, ids[len(ids)-1], nil
}

// ProviderEventIDBefore returns the highest id such that every archived event
// with an id in (afterID, that id] was received before cutoff: the id just
// below the first one received at or after it, or the newest id when none was.
// Ids only grow (AUTOINCREMENT), so the rows it vouches for stay that old. It
// reads the ids from afterID up to that first younger row and no further.
func (s *Store) ProviderEventIDBefore(ctx context.Context, afterID int64, cutoff time.Time) (int64, error) {
	var first int64
	err := s.readDB.QueryRowContext(ctx, `
SELECT e.id
FROM conversation_provider_events AS e
WHERE e.id > ? AND e.received_at >= ?
ORDER BY e.id ASC
LIMIT 1`, afterID, receivedAtCutoff(cutoff)).Scan(&first)
	if err == nil {
		return first - 1, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("provider events received before %s: %w", receivedAtCutoff(cutoff), err)
	}
	var newest sql.NullInt64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT MAX(id) FROM conversation_provider_events`).Scan(&newest); err != nil {
		return 0, fmt.Errorf("provider events received before %s: newest: %w", receivedAtCutoff(cutoff), err)
	}
	if !newest.Valid || newest.Int64 < afterID {
		return afterID, nil
	}
	return newest.Int64, nil
}

func (s *Store) deleteSettledProviderDeltas(ctx context.Context, idList string, args []any) (int64, error) {
	// The predicate is checked again: a turn may have opened since the read.
	res, err := s.writeDB.ExecContext(ctx, `
DELETE FROM conversation_provider_events
WHERE id IN (
    SELECT e.id
    FROM conversation_provider_events AS e
    WHERE e.id IN (SELECT value FROM json_each(?))
      AND `+settledDeltaPredicate+`
)`, append([]any{idList}, args...)...)
	if err != nil {
		return 0, fmt.Errorf("prune settled provider deltas: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune settled provider deltas: rows affected: %w", err)
	}
	return n, nil
}
