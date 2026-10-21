-- +goose Up
-- A streamed delta from a persistent chat host was archived as its own
-- conversation_provider_events row (plus three index entries) only so a host replay could not
-- apply it twice, then pruned hours later: two writes per token chunk. A host numbers its events
-- in order and AO applies them in that order, so the newest applied position per conversation and
-- host answers the same question in one row.
CREATE TABLE conversation_provider_delta_marks (
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    seq INTEGER NOT NULL,
    idx INTEGER NOT NULL,
    PRIMARY KEY (conversation_id, source)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE conversation_provider_delta_marks;
