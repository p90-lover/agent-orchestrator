package store

import (
	"context"
	"fmt"
	"time"
)

// providerDeltaMethods are the per-chunk streaming events. They are the bulk
// of conversation_provider_events (one row per reasoning or message token
// chunk: 1.1M reasoning.delta rows, 718 MB, in a single day of one chat) and
// the projection keeps what they built: the message, the reasoning, the
// command's (truncated) output.
var providerDeltaMethods = []string{"reasoning.delta", "message.delta", "command.output.delta"}

// receivedAtCutoff renders a cutoff the way received_at is stored (the
// modernc driver writes time.Time as Go's String form, "2006-01-02
// 15:04:05.9999999 +0000 UTC"), so the text comparison orders correctly.
func receivedAtCutoff(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// PruneSettledProviderDeltas removes at most limit streaming-chunk rows from
// the provider event archive: those older than settledBefore whose
// conversation has no queued or running turn, and any older than anyBefore.
//
// The archive has no production reader; it is evidence of what the provider
// sent, and its event identity deduplicates events a host replays before the
// turn's terminal event is acknowledged. Once no turn of the conversation is
// open, nothing can replay those chunks, so they only cost space and every
// write's index maintenance. Turn, item, approval and usage events are kept.
func (s *Store) PruneSettledProviderDeltas(ctx context.Context, settledBefore, anyBefore time.Time, limit int64) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return 0, err
	}
	defer s.writeMu.Unlock()
	res, err := s.writeDB.ExecContext(ctx, `
DELETE FROM conversation_provider_events
WHERE id IN (
    SELECT e.id
    FROM conversation_provider_events AS e
    WHERE e.method IN (?, ?, ?)
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
      )
    ORDER BY e.id ASC
    LIMIT ?
)`,
		providerDeltaMethods[0], providerDeltaMethods[1], providerDeltaMethods[2],
		receivedAtCutoff(anyBefore), receivedAtCutoff(settledBefore), limit)
	if err != nil {
		return 0, fmt.Errorf("prune settled provider deltas: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune settled provider deltas: rows affected: %w", err)
	}
	return n, nil
}
