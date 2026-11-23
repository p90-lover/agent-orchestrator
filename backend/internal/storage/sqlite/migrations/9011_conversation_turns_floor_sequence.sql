-- +goose Up
-- A turn's own messages are looked up by turn_id on every completed turn
-- (SettleStreamingConversationMessagesForTurn, inside the projection's writer
-- transaction), and when a queued prompt is edited, a prompt is retried or
-- edited, and a forked chat's timeline asks which turns came before the fork.
-- No index on conversation_messages leads with turn_id, so each of those walked
-- the chat's (conversation_id, sequence) index from its first message: about
-- 9,100 page reads (37 MB) per lookup in a 12,000-message chat, and the lineage
-- check scanned every chat's messages. An index that would answer it, even a
-- partial one of streaming rows, has to read that whole table to be built.
--
-- floor_sequence is the chat's latest_sequence when the turn row is written.
-- Every message and activity takes its sequence from that same counter, and a
-- turn's rows are written after the turn row (its prompt takes the sequence in
-- the same transaction, just before), so no row of the turn has a lower
-- sequence. The lookups start the chat's sequence index at the floor: the
-- turn's rows are then the first ones read, not the last.
--
-- Adding a column rewrites no row. Turns written before this have no floor and
-- the lookups read them from sequence 0, as before.
ALTER TABLE conversation_turns ADD COLUMN floor_sequence INTEGER;

-- +goose Down
ALTER TABLE conversation_turns DROP COLUMN floor_sequence;
