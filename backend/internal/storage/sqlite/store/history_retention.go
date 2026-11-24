package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Chat history retention (cdc.HistoryRetention). On 2026-11-24 ao.db was
// 6.6 GB and grew about 0.22 GB a day on a disk with 7 GB free: the raw
// provider-event archive was 4.3 GB of it and tool-card bodies 1.9 GB, while
// the chat text itself was 0.1 GB. These methods bound the two bulky parts and
// give freed pages back to the disk; each write goes through maintain, so a
// chat write never waits behind them for long.

// TrimmedTextNote ends every text the retention cut short, so a reader of an
// old card sees that the rest was removed rather than a silently shorter text.
const TrimmedTextNote = "\n… [older text trimmed by AO to save disk space]"

// rowBefore describes how to find the newest row older than a cutoff in a
// table whose ids grow in arrival order. The time column comes after the
// table's bulky text, so reading it walks that row's overflow pages: a binary
// search reads it from a couple of dozen rows instead of from every row.
type rowBefore struct {
	table, id, at string
}

var (
	providerArchiveRows = rowBefore{table: "conversation_provider_events", id: "id", at: "received_at"}
	activityRows        = rowBefore{table: "conversation_activities", id: "rowid", at: "created_at"}
)

// idBefore returns the highest id after afterID whose row arrived before
// cutoff, or afterID when none did. Ids grow with arrival (AUTOINCREMENT ids,
// rowids of rows never deleted from the end), so the rows up to the answer are
// the old ones; the writes that use it check the time again row by row.
func (s *Store) idBefore(ctx context.Context, r rowBefore, afterID int64, cutoff time.Time) (int64, error) {
	var newest sql.NullInt64
	if err := s.readDB.QueryRowContext(ctx, `SELECT MAX(`+r.id+`) FROM `+r.table).Scan(&newest); err != nil {
		return 0, fmt.Errorf("%s rows before %s: newest: %w", r.table, receivedAtCutoff(cutoff), err)
	}
	if !newest.Valid || newest.Int64 <= afterID {
		return afterID, nil
	}
	probe := `SELECT ` + r.id + `, ` + r.at + ` < ? FROM ` + r.table + ` WHERE ` + r.id + ` >= ? ORDER BY ` + r.id + ` LIMIT 1`
	at := receivedAtCutoff(cutoff)
	// Every row in (afterID, lo] is old and every row from hi on is young.
	lo, hi := afterID, newest.Int64+1
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		var id int64
		var old bool
		err := s.readDB.QueryRowContext(ctx, probe, at, mid).Scan(&id, &old)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			hi = mid
		case err != nil:
			return 0, fmt.Errorf("%s rows before %s: %w", r.table, at, err)
		case id >= hi || !old:
			// No row in [mid, hi), or the first one there is young.
			hi = mid
		default:
			lo = id
		}
	}
	return lo, nil
}

// ProviderEventIDReceivedBefore returns the highest archive id after afterID
// received before cutoff (afterID when none was).
func (s *Store) ProviderEventIDReceivedBefore(ctx context.Context, afterID int64, cutoff time.Time) (int64, error) {
	return s.idBefore(ctx, providerArchiveRows, afterID, cutoff)
}

// ActivityRowIDCreatedBefore returns the highest conversation_activities rowid
// after afterRowID created before cutoff (afterRowID when none was).
func (s *Store) ActivityRowIDCreatedBefore(ctx context.Context, afterRowID int64, cutoff time.Time) (int64, error) {
	return s.idBefore(ctx, activityRows, afterRowID, cutoff)
}

// providerEventIDReplays reports whether an archive event id can arrive again
// once its turn is over, so that its row must stay to deduplicate it.
//
// An empty id is never deduplicated (projectProviderEventTx). A persistent
// host's id ("acp-host:<identity>:<seq>[:<idx>]") carries the host process's
// random identity and is replayed only from the journal of a turn the host has
// not finished, so once that turn is over nothing can send it again. Any other
// id is derived from what the provider stored: a history import
// ("acp-history:<content hash>", Codex "...history...") sends the same ids every
// time the chat is loaded again, and only the archive row stops AO from
// projecting that history a second time.
func providerEventIDReplays(id string) bool {
	return id != "" && !strings.HasPrefix(id, "acp-host:")
}

// archiveSweepOpenTurnGuard keeps rows (alias e) a still-open turn could
// replay: those received after an open turn of their conversation began. Its
// parameter is nothing; it reads the row's own columns.
const archiveSweepOpenTurnGuard = `NOT EXISTS (
    SELECT 1 FROM conversation_turns AS t
    WHERE t.conversation_id = e.conversation_id
      AND t.requested_at <= e.received_at
      AND t.state IN ('queued', 'running')
)`

// SweepProviderEventArchive retires up to limit archive rows with an id in
// (afterID, throughID] that were received before receivedBefore:
//
//   - rows whose event id can never arrive again (empty, or a persistent
//     host's) are deleted;
//   - rows whose id a history import sends again keep the row, and so the
//     dedupe key, with an empty payload.
//
// The archive has no production reader (ProviderEventsSince is for tests and
// debugging); it is evidence of what the provider sent, worth keeping for a
// week of debugging, and the only dedupe for replayed events. Rows received
// after an open turn of their conversation began are left alone.
//
// The rows are read on a read connection, from the columns that come before
// the payload, so the read opens no overflow page; only the writes take the
// writer, through maintain. found is how many rows the read returned and lastID
// the highest of them: a caller continues after lastID, and a found below
// limit means the range is done.
func (s *Store) SweepProviderEventArchive(ctx context.Context, receivedBefore time.Time, afterID, throughID, limit int64) (found, deleted, emptied, lastID int64, err error) {
	if limit <= 0 || throughID <= afterID {
		return 0, 0, 0, 0, nil
	}
	rows, err := s.readDB.QueryContext(ctx, `
SELECT id, provider_event_id, octet_length(payload_json) > 0
FROM conversation_provider_events
WHERE id > ? AND id <= ?
ORDER BY id
LIMIT ?`, afterID, throughID, limit)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("sweep provider event archive: read: %w", err)
	}
	var drop, empty []int64
	for rows.Next() {
		var id int64
		var eventID string
		var hasPayload bool
		if err := rows.Scan(&id, &eventID, &hasPayload); err != nil {
			_ = rows.Close()
			return 0, 0, 0, 0, fmt.Errorf("sweep provider event archive: read: %w", err)
		}
		found++
		lastID = id
		switch {
		case !providerEventIDReplays(eventID):
			drop = append(drop, id)
		case hasPayload:
			empty = append(empty, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("sweep provider event archive: read: %w", err)
	}
	cutoff := receivedAtCutoff(receivedBefore)
	if len(drop) > 0 {
		deleted, err = s.maintain(ctx, func(ctx context.Context) (int64, error) {
			return s.execIDList(ctx, `
DELETE FROM conversation_provider_events
WHERE id IN (
    SELECT e.id FROM conversation_provider_events AS e
    WHERE e.id IN (SELECT value FROM json_each(?))
      AND e.received_at < ?
      AND `+archiveSweepOpenTurnGuard+`
)`, drop, cutoff)
		})
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	if len(empty) > 0 {
		emptied, err = s.maintain(ctx, func(ctx context.Context) (int64, error) {
			return s.execIDList(ctx, `
UPDATE conversation_provider_events SET payload_json = ''
WHERE id IN (
    SELECT e.id FROM conversation_provider_events AS e
    WHERE e.id IN (SELECT value FROM json_each(?))
      AND e.received_at < ?
      AND octet_length(e.payload_json) > 0
      AND `+archiveSweepOpenTurnGuard+`
)`, empty, cutoff)
		})
		if err != nil {
			return 0, 0, 0, 0, err
		}
	}
	return found, deleted, emptied, lastID, nil
}

// execIDList runs a write whose first parameter is a JSON array of ids.
func (s *Store) execIDList(ctx context.Context, query string, ids []int64, args ...any) (int64, error) {
	list, err := json.Marshal(ids)
	if err != nil {
		return 0, err
	}
	res, err := s.writeDB.ExecContext(ctx, query, append([]any{string(list)}, args...)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TrimActivityBodies shortens the bulky text of up to limit tool cards with a
// rowid in (afterRowID, throughRowID] created before createdBefore: every
// text value inside detail_json and the stored command output longer than
// maxText bytes keeps its first maxText bytes and ends with TrimmedTextNote,
// and trimmed command output is marked truncated. Kind, status, summary, ids,
// order and every shorter value stay as they were.
//
// The revision is left alone on purpose: the conversation_activities update
// trigger emits a change event only when it moves, so trimming months-old
// cards wakes no open chat. Cached copies of a trimmed row are dropped.
//
// Only rows with a long detail or output are read (octet_length reads the
// record header, not the text). found and lastRowID follow
// SweepProviderEventArchive: a found below limit means the range is done.
func (s *Store) TrimActivityBodies(ctx context.Context, createdBefore time.Time, afterRowID, throughRowID, limit int64, maxText int) (found, trimmed, lastRowID int64, err error) {
	if limit <= 0 || throughRowID <= afterRowID || maxText <= 0 {
		return 0, 0, 0, nil
	}
	rows, err := s.readDB.QueryContext(ctx, `
SELECT rowid, id, revision, detail_json, command_output
FROM conversation_activities
WHERE rowid > ? AND rowid <= ?
  AND (octet_length(detail_json) > ? OR octet_length(command_output) > ?)
ORDER BY rowid
LIMIT ?`, afterRowID, throughRowID, maxText, maxText, limit)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("trim activity bodies: read: %w", err)
	}
	type update struct {
		rowID, revision int64
		id              string
		detail, output  sql.NullString
	}
	var updates []update
	for rows.Next() {
		var u update
		var detail, output string
		if err := rows.Scan(&u.rowID, &u.id, &u.revision, &detail, &output); err != nil {
			_ = rows.Close()
			return 0, 0, 0, fmt.Errorf("trim activity bodies: read: %w", err)
		}
		found++
		lastRowID = u.rowID
		if d, ok := trimDetailJSON(detail, maxText); ok {
			u.detail = sql.NullString{String: d, Valid: true}
		}
		if o, ok := trimText(output, maxText); ok {
			u.output = sql.NullString{String: o, Valid: true}
		}
		if u.detail.Valid || u.output.Valid {
			updates = append(updates, u)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, 0, fmt.Errorf("trim activity bodies: read: %w", err)
	}
	if len(updates) == 0 {
		return found, 0, lastRowID, nil
	}
	cutoff := receivedAtCutoff(createdBefore)
	trimmed, err = s.maintain(ctx, func(ctx context.Context) (int64, error) {
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		stmt, err := tx.PrepareContext(ctx, `
UPDATE conversation_activities
SET detail_json = coalesce(?, detail_json),
    command_output = coalesce(?, command_output),
    command_output_truncated = CASE WHEN ? THEN 1 ELSE command_output_truncated END
WHERE rowid = ? AND revision = ? AND created_at < ?`)
		if err != nil {
			return 0, err
		}
		defer func() { _ = stmt.Close() }()
		var n int64
		for _, u := range updates {
			res, err := stmt.ExecContext(ctx, u.detail, u.output, u.output.Valid, u.rowID, u.revision, cutoff)
			if err != nil {
				return 0, err
			}
			k, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			n += k
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return n, nil
	})
	if err != nil {
		return 0, 0, 0, err
	}
	for _, u := range updates {
		s.activityRows.drop(u.id)
	}
	return found, trimmed, lastRowID, nil
}

// trimText cuts s to at most keep bytes, on a character boundary, and appends
// TrimmedTextNote. It reports false, and returns s, when s is short enough or
// was already trimmed.
func trimText(s string, keep int) (string, bool) {
	if len(s) <= keep || strings.HasSuffix(s, TrimmedTextNote) && len(s) <= keep+len(TrimmedTextNote) {
		return s, false
	}
	cut := keep
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TrimmedTextNote, true
}

// trimDetailJSON applies trimText to every string value in a JSON document,
// at any depth. It reports false, and returns the document unchanged, when no
// value was cut or the document is not JSON: an unknown shape is kept rather
// than guessed at. Numbers keep their exact text; object keys come back
// sorted, which no reader of these documents depends on.
func trimDetailJSON(doc string, keep int) (string, bool) {
	if len(doc) <= keep {
		return doc, false
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return doc, false
	}
	v, changed := trimJSONValue(v, keep)
	if !changed {
		return doc, false
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return doc, false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

func trimJSONValue(v any, keep int) (any, bool) {
	switch t := v.(type) {
	case string:
		return trimText(t, keep)
	case []any:
		changed := false
		for i, item := range t {
			next, ok := trimJSONValue(item, keep)
			if ok {
				t[i], changed = next, true
			}
		}
		return t, changed
	case map[string]any:
		changed := false
		for k, item := range t {
			next, ok := trimJSONValue(item, keep)
			if ok {
				t[k], changed = next, true
			}
		}
		return t, changed
	default:
		return v, false
	}
}

// FreePages reports the database's free pages, its page size and whether it
// is in incremental auto-vacuum mode (the app's launcher switches ao.db to it),
// the only mode in which IncrementalVacuumStep gives pages back.
func (s *Store) FreePages(ctx context.Context) (free, pageSize int64, incremental bool, err error) {
	var mode int64
	if err := s.readDB.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, 0, false, fmt.Errorf("read auto_vacuum: %w", err)
	}
	if err := s.readDB.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		return 0, 0, false, fmt.Errorf("read freelist_count: %w", err)
	}
	if err := s.readDB.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, false, fmt.Errorf("read page_size: %w", err)
	}
	return free, pageSize, mode == 2, nil
}

// IncrementalVacuumStep gives up to pages free pages back to the file system
// (PRAGMA incremental_vacuum), under the writer through maintain: freeing a
// page at the end of the file can mean moving a live page into a free slot,
// which is random I/O on a hard disk, so steps stay small and yield to chat
// writes. It returns how many pages it freed; an interrupted step frees none.
func (s *Store) IncrementalVacuumStep(ctx context.Context, pages int64) (int64, error) {
	if pages <= 0 {
		return 0, nil
	}
	return s.maintain(ctx, func(ctx context.Context) (int64, error) {
		// The pragma returns one row per page it frees, and keeps freeing only
		// while it is stepped, so read every row.
		rows, err := s.writeDB.QueryContext(ctx, `PRAGMA incremental_vacuum(`+strconv.FormatInt(pages, 10)+`)`)
		if err != nil {
			return 0, err
		}
		var n int64
		for rows.Next() {
			n++
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return 0, err
		}
		return n, nil
	})
}

// RetentionCursor returns how far the named retention sweep has got (0 when it
// has not run). Sweeps keep their position here so an AO restart does not
// read again what earlier passes already handled.
func (s *Store) RetentionCursor(ctx context.Context, name string) (int64, error) {
	var position int64
	err := s.readDB.QueryRowContext(ctx, `SELECT position FROM history_retention_cursors WHERE name = ?`, name).Scan(&position)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read retention cursor %s: %w", name, err)
	}
	return position, nil
}

// SetRetentionCursor records a sweep's position; it never moves one back.
func (s *Store) SetRetentionCursor(ctx context.Context, name string, position int64, now time.Time) error {
	_, err := s.maintain(ctx, func(ctx context.Context) (int64, error) {
		res, err := s.writeDB.ExecContext(ctx, `
INSERT INTO history_retention_cursors (name, position, updated_at) VALUES (?, ?, ?)
ON CONFLICT (name) DO UPDATE SET position = excluded.position, updated_at = excluded.updated_at
WHERE excluded.position > history_retention_cursors.position`, name, position, now.UTC())
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
	if err != nil {
		return fmt.Errorf("record retention cursor %s: %w", name, err)
	}
	return nil
}
