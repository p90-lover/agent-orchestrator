-- +goose Up
-- How far each chat history retention sweep (cdc.HistoryRetention) has got: the
-- provider-event archive sweep and the old tool-card trim walk their tables in
-- id order and keep their place here. Without it every AO start would walk the
-- handled part of conversation_activities again, gigabytes on a hard disk, to
-- find nothing left to do. A new table rewrites no existing row.
CREATE TABLE history_retention_cursors (
    name       TEXT PRIMARY KEY,
    position   INTEGER NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE history_retention_cursors;
