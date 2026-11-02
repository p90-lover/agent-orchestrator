-- +goose Up
-- When a chat controller stops, its leftover work is settled: the session's
-- queued or running turns, and the running activities of those turns. Neither
-- table had an index that answers that, so each stop scanned every turn and
-- every activity of every chat while holding the single writer; after a
-- restart resumed ten chats this was 89% of the daemon's CPU samples, a full
-- pass over conversation_activities about once a minute, and turn stops queued
-- behind it (2026-10-31). Only open work is indexed, a handful of rows at any
-- time, so these cost almost nothing to keep.
CREATE INDEX idx_conversation_turns_open_by_session
    ON conversation_turns (handled_by_session_id)
    WHERE state IN ('queued', 'running');
CREATE INDEX idx_conversation_activities_running_by_turn
    ON conversation_activities (turn_id)
    WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS idx_conversation_activities_running_by_turn;
DROP INDEX IF EXISTS idx_conversation_turns_open_by_session;
