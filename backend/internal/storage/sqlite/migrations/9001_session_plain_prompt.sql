-- +goose Up
-- plain_prompt starts a worker without AO's standing system prompt; restores keep it.
ALTER TABLE sessions ADD COLUMN plain_prompt INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE sessions DROP COLUMN plain_prompt;
