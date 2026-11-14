-- +goose Up
-- When a chat's turn completes, its controller asks for the next queued
-- message while it holds the dispatch lock, so a send to that chat waits for
-- the answer. idx_conversation_turns_conversation is ordered by
-- (conversation_id, requested_at) but knows nothing of state, so answering
-- "nothing is queued", the usual answer, meant reading every turn row the chat
-- ever had. This index holds only turns still waiting to be delivered, a
-- handful at any time: the empty answer is one probe, and a queued head is
-- the first entry. Building it reads conversation_turns once, as 9006 did;
-- conversation_messages, the large table, is not indexed here.
CREATE INDEX idx_conversation_turns_queue_head
    ON conversation_turns (conversation_id, requested_at)
    WHERE state = 'queued' AND promotion_started_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_conversation_turns_queue_head;
