-- +goose Up
-- The provider-chunk janitor (cdc.ProviderRetentionJanitor) deletes streaming
-- chunks from conversation_provider_events and keeps every other event. To find
-- where "received before the cutoff" ends it walked the archive in id order
-- and read received_at from each row. received_at follows payload_json, so every
-- kept turn, item, approval and usage event was read with its payload's overflow
-- chain. Its starting point lives in memory, so after each AO start the walk
-- began at the first row: on 2026-11-16 it read for over 17 minutes and was 19%
-- of the daemon's samples, and every later pass reread about 18 hours of rows.
--
-- Only chunk rows can be deleted, so only they decide where a pass may start
-- and stop. This index holds the chunk rows alone, which the janitor keeps few,
-- and every column both janitor reads need, so neither read opens a row. It
-- carries method, the column of its own condition, because SQLite reads that
-- again from the row unless the index has it. Building it reads each row's
-- first page for method and never a kept event's overflow chain.
CREATE INDEX idx_conversation_provider_events_chunks
    ON conversation_provider_events (id, received_at, conversation_id, method)
    WHERE method IN ('reasoning.delta', 'message.delta', 'command.output.delta');

-- +goose Down
DROP INDEX IF EXISTS idx_conversation_provider_events_chunks;
