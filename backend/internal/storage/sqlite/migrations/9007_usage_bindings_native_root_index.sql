-- +goose Up
-- Every new usage event asks whether another binding of the same native
-- transcript already recorded it (CountUsageEventForNativeRootElsewhere).
-- usage_bindings had no index that finds a native root's bindings, so SQLite
-- answered by walking the whole (binding_id, source_event_key) index of
-- model_usage_events: every usage event ever recorded, once per new event,
-- inside the writer's transaction. With six chats working this was the
-- daemon's largest reader, 37.5% of its SQLite page-read time, and it grows
-- with the usage history (2026-11-02). A native root has a handful of
-- bindings, so this index turns the walk into a few page reads.
CREATE INDEX idx_usage_bindings_native_root
    ON usage_bindings (harness, native_root_id);

-- +goose Down
DROP INDEX IF EXISTS idx_usage_bindings_native_root;
