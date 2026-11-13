package store

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ConversationReplayRows is what a native-history import reconciles a provider's
// replayed thread against: the turns, messages and activities a full snapshot
// returns, without the columns the import never reads.
type ConversationReplayRows struct {
	Turns      []domain.ConversationTurn
	Messages   []domain.ConversationMessage
	Activities []domain.ConversationActivity
}

// replayMessagesSQL is SelectConversationMessages (queries/conversations.sql)
// without delivery_content_json, and replayActivitiesSQL is
// SelectConversationActivities without command_output, streamed_text and their
// truncated flags. Keep each in step with its generated query: a test runs both
// on the same conversation and requires the same rows, in the same order, with
// the same values apart from the dropped columns.
//
// Those columns sit after every column the import reads, and SQLite stores a
// row's columns in order, so a read that stops before them never touches their
// overflow pages. branch_id is the last column of both tables. The CROSS JOIN
// keeps the active path as the outer loop and INDEXED BY makes the inner lookup
// go through (branch_id, sequence), so branch_id comes from the index and is
// never read from the row; if the index is ever dropped, the statement fails to
// prepare instead of silently reading every row to its end.
const replayActivePathSQL = `
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
)`

const replayMessagesSQL = replayActivePathSQL + `
SELECT message.id, message.conversation_id, message.turn_id, message.sequence, message.revision,
    message.role, message.origin, message.text, message.streaming, message.provider_item_id,
    message.client_message_id, message.created_at, message.updated_at
FROM active_path AS path
CROSS JOIN conversation_messages AS message INDEXED BY idx_conversation_messages_branch
WHERE message.branch_id = path.branch_id
  AND message.conversation_id = ?1
  AND (path.max_sequence IS NULL OR message.sequence <= path.max_sequence)
  AND (message.turn_id IS NULL OR message.turn_id NOT IN (
      SELECT discarded.id FROM conversation_turns AS discarded
      WHERE discarded.conversation_id = ?1
        AND (
          discarded.rolled_back_at IS NOT NULL
          OR discarded.promoted_to_turn_id IS NOT NULL
          OR discarded.state = 'cancelled'
        )
  ))
ORDER BY message.sequence`

const replayActivitiesSQL = replayActivePathSQL + `
SELECT activity.id, activity.conversation_id, activity.turn_id, activity.sequence, activity.revision,
    activity.kind, activity.status, activity.summary, activity.detail_json, activity.request_id,
    activity.provider_item_id, activity.created_at, activity.updated_at
FROM active_path AS path
CROSS JOIN conversation_activities AS activity INDEXED BY idx_conversation_activities_branch
WHERE activity.branch_id = path.branch_id
  AND activity.conversation_id = ?1
  AND (path.max_sequence IS NULL OR activity.sequence <= path.max_sequence)
  AND (activity.turn_id IS NULL OR activity.turn_id NOT IN (
      SELECT discarded.id FROM conversation_turns AS discarded
      WHERE discarded.conversation_id = ?1
        AND (
          discarded.rolled_back_at IS NOT NULL
          OR discarded.state = 'cancelled'
        )
  ))
ORDER BY activity.sequence`

// LoadConversationReplayRows reads the rows a native-history import reconciles
// against. Turns are built exactly as LoadConversationSnapshot builds them;
// messages and activities are the snapshot's rows without
// DeliveryContentJSON, CommandOutput, StreamedText and the truncated flags.
//
// Resuming a chat after its provider process died imports the provider's native
// thread, and the import needs each message's text and each activity's detail
// for its content fingerprints, but none of those columns. Long chats carry
// megabytes of command output and streamed text; after an AO restart on a busy
// disk, reading them made a resume time out and held up every other read.
func (s *Store) LoadConversationReplayRows(ctx context.Context, conversationID string) (ConversationReplayRows, error) {
	turns, err := s.LoadConversationTurns(ctx, conversationID)
	if err != nil {
		return ConversationReplayRows{}, err
	}
	messages, err := s.selectReplayMessages(ctx, conversationID)
	if err != nil {
		return ConversationReplayRows{}, fmt.Errorf("select messages: %w", err)
	}
	activities, err := s.selectReplayActivities(ctx, conversationID)
	if err != nil {
		return ConversationReplayRows{}, fmt.Errorf("select activities: %w", err)
	}
	return ConversationReplayRows{Turns: turns, Messages: messages, Activities: activities}, nil
}

func (s *Store) selectReplayMessages(ctx context.Context, conversationID string) ([]domain.ConversationMessage, error) {
	rows, err := s.readDB.QueryContext(ctx, replayMessagesSQL, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.ConversationMessage
	for rows.Next() {
		var i gen.ConversationMessage
		if err := rows.Scan(
			&i.ID,
			&i.ConversationID,
			&i.TurnID,
			&i.Sequence,
			&i.Revision,
			&i.Role,
			&i.Origin,
			&i.Text,
			&i.Streaming,
			&i.ProviderItemID,
			&i.ClientMessageID,
			&i.CreatedAt,
			&i.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, messageToDomain(i))
	}
	return out, rows.Err()
}

func (s *Store) selectReplayActivities(ctx context.Context, conversationID string) ([]domain.ConversationActivity, error) {
	rows, err := s.readDB.QueryContext(ctx, replayActivitiesSQL, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.ConversationActivity
	for rows.Next() {
		var i gen.ConversationActivity
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		out = append(out, activityToDomain(i))
	}
	return out, rows.Err()
}
