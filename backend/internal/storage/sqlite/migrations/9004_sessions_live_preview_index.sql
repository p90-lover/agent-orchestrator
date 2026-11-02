-- +goose Up
-- The preview poller lists live sessions four times a second. With no index
-- that answers it, each poll read every sessions leaf page and, for each live
-- row, the overflow pages holding its prompt, which sit in front of the
-- preview columns: about 150 pages a poll, re-read from disk because any write
-- resets the readers' page cache. This partial index holds exactly what the
-- poll selects, in its order, so a poll reads a handful of index pages.
-- Rows change these columns rarely, so it costs almost nothing to keep.
CREATE INDEX idx_sessions_live_preview
    ON sessions (project_id, num, id, kind, workspace_path, preview_url, preview_revision)
    WHERE is_terminated = 0;

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_live_preview;
