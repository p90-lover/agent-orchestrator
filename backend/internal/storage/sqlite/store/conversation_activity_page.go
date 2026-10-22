package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// activityPageKeysSQL is SelectConversationActivitiesPage (queries/conversations.sql)
// returning only each row's id and revision. Keep the two in step: a test runs
// both on the same conversations and requires the same rows in the same order.
//
// A snapshot page is polled for every working chat card, and its activity rows
// carry command output and streamed text: a 51-row page of a busy chat was
// ~390 KiB, read from disk on every poll because any write resets the readers'
// page cache. Rows that did not change since the last poll are served from
// activityRowCache instead; only new or changed ones are read in full.
const activityPageKeysSQL = `
WITH RECURSIVE active_path(branch_id, max_sequence) AS (
    SELECT conversations.active_branch_id, CAST(NULL AS INTEGER)
    FROM conversations
    WHERE conversations.id = ?1
    UNION ALL
    SELECT branch.parent_branch_id,
           CASE
               WHEN path.max_sequence IS NULL THEN branch.fork_after_sequence
               WHEN branch.fork_after_sequence < path.max_sequence THEN branch.fork_after_sequence
               ELSE path.max_sequence
           END
    FROM active_path AS path
    JOIN conversation_branches AS branch ON branch.id = path.branch_id
    WHERE branch.parent_branch_id IS NOT NULL
)
SELECT conversation_activities.id, conversation_activities.revision FROM conversation_activities
JOIN active_path AS path ON path.branch_id = conversation_activities.branch_id
WHERE conversation_activities.conversation_id = ?1
  AND conversation_activities.sequence < ?2
  AND (path.max_sequence IS NULL OR conversation_activities.sequence <= path.max_sequence)
  AND (conversation_activities.turn_id IS NULL OR conversation_activities.turn_id NOT IN (
      SELECT discarded.id FROM conversation_turns AS discarded
      WHERE discarded.conversation_id = ?1
        AND (
          discarded.rolled_back_at IS NOT NULL
          OR discarded.state = 'cancelled'
        )
  ))
ORDER BY conversation_activities.sequence DESC
LIMIT ?3`

const activityRowByIDSQL = `
SELECT id, conversation_id, turn_id, sequence, revision, kind, status, summary,
    detail_json, request_id, provider_item_id, created_at, updated_at,
    command_output, command_output_truncated, streamed_text, streamed_text_truncated, branch_id
FROM conversation_activities WHERE id = ?`

// activityRowCache keeps activity rows by id. A row is reused only while its
// revision is the one the page query reports: every UPDATE of
// conversation_activities bumps revision, so an unchanged revision is an
// unchanged row. It is bounded by the bytes it holds and starts over when full,
// which only costs the reads it saved.
type activityRowCache struct {
	mu       sync.Mutex
	rows     map[string]gen.ConversationActivity
	bytes    int
	maxBytes int
}

const activityRowCacheBytes = 32 << 20

func newActivityRowCache(maxBytes int) *activityRowCache {
	return &activityRowCache{rows: map[string]gen.ConversationActivity{}, maxBytes: maxBytes}
}

func activityRowBytes(row gen.ConversationActivity) int {
	return len(row.ID) + len(row.Summary) + len(row.DetailJson) + len(row.CommandOutput) + len(row.StreamedText) + 256
}

func (c *activityRowCache) get(id string, revision int64) (gen.ConversationActivity, bool) {
	if c == nil {
		return gen.ConversationActivity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[id]
	if !ok || row.Revision != revision {
		return gen.ConversationActivity{}, false
	}
	return row, true
}

func (c *activityRowCache) put(row gen.ConversationActivity) {
	if c == nil {
		return
	}
	size := activityRowBytes(row)
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.rows[row.ID]; ok {
		c.bytes -= activityRowBytes(old)
	}
	if c.bytes+size > c.maxBytes {
		c.rows = map[string]gen.ConversationActivity{}
		c.bytes = 0
	}
	c.rows[row.ID] = row
	c.bytes += size
}

type activityPageKey struct {
	id       string
	revision int64
}

// selectConversationActivitiesPage returns what SelectConversationActivitiesPage
// returns, reading in full only the rows the cache does not hold at their
// current revision.
func (s *Store) selectConversationActivitiesPage(ctx context.Context, arg gen.SelectConversationActivitiesPageParams) ([]gen.ConversationActivity, error) {
	keyRows, err := s.readDB.QueryContext(ctx, activityPageKeysSQL, arg.ConversationID, arg.BeforeSequence, arg.PageLimit)
	if err != nil {
		return nil, err
	}
	var keys []activityPageKey
	for keyRows.Next() {
		var key activityPageKey
		if err := keyRows.Scan(&key.id, &key.revision); err != nil {
			_ = keyRows.Close()
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := keyRows.Close(); err != nil {
		return nil, err
	}
	if err := keyRows.Err(); err != nil {
		return nil, err
	}
	out := make([]gen.ConversationActivity, 0, len(keys))
	for _, key := range keys {
		if row, ok := s.activityRows.get(key.id, key.revision); ok {
			out = append(out, row)
			continue
		}
		row, err := s.selectActivityRowByID(ctx, key.id)
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted since the key read (a discarded branch): the next poll
			// no longer lists it either.
			continue
		}
		if err != nil {
			return nil, err
		}
		s.activityRows.put(row)
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) selectActivityRowByID(ctx context.Context, id string) (gen.ConversationActivity, error) {
	var i gen.ConversationActivity
	err := s.readDB.QueryRowContext(ctx, activityRowByIDSQL, id).Scan(
		&i.ID,
		&i.ConversationID,
		&i.TurnID,
		&i.Sequence,
		&i.Revision,
		&i.Kind,
		&i.Status,
		&i.Summary,
		&i.DetailJson,
		&i.RequestID,
		&i.ProviderItemID,
		&i.CreatedAt,
		&i.UpdatedAt,
		&i.CommandOutput,
		&i.CommandOutputTruncated,
		&i.StreamedText,
		&i.StreamedTextTruncated,
		&i.BranchID,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("select activity %s: %w", id, err)
	}
	return i, err
}
