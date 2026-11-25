-- +goose Up
-- fast_mode pins a chat's fast output choice (Codex's "fast" service tier,
-- Claude Code's fastMode setting) so a resumed chat keeps it, as effort is
-- pinned. A constant default adds the column without rewriting any row.
ALTER TABLE sessions ADD COLUMN fast_mode INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN fast_mode;
