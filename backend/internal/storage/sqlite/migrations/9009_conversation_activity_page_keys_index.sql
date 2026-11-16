-- +goose Up
-- A chat card's snapshot poll lists the newest activities of the chat's active
-- branch (activityPageKeysSQL). It walked idx_conversation_activities_order and
-- opened each activity row for turn_id, revision and branch_id. branch_id is the
-- row's last column, after detail_json, command_output and streamed_text, so a
-- busy chat's poll read a table page and an overflow chain per activity; on
-- 2026-11-16 that was 46% of the daemon's samples with the disk 100% busy. The
-- same poll opened every turn the chat ever had to find the discarded ones.
--
-- The page keys now come from indexes alone: this index for the walk, newest
-- first, and idx_conversation_activities_branch for the branch, probed by
-- (branch_id, sequence, rowid). Every column here precedes the large ones, so
-- building the index reads each row's first page and never its overflow chain.
-- The discarded-turn index holds rolled-back and cancelled turns only, a handful
-- per chat. It carries the two columns of its own condition because SQLite
-- reads them again from the row unless the index has them.
CREATE INDEX idx_conversation_activities_page_keys
    ON conversation_activities (conversation_id, sequence, turn_id, revision, id);
CREATE INDEX idx_conversation_turns_discarded
    ON conversation_turns (conversation_id, id, rolled_back_at, state)
    WHERE rolled_back_at IS NOT NULL OR state = 'cancelled';

-- +goose Down
DROP INDEX IF EXISTS idx_conversation_turns_discarded;
DROP INDEX IF EXISTS idx_conversation_activities_page_keys;
