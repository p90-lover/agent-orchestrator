-- +goose Up
-- This migration used to build idx_conversation_provider_events_chunks, the
-- index the provider-chunk janitor (cdc.ProviderRetentionJanitor) reads instead
-- of the archive. It builds nothing now: Store.EnsureProviderChunkIndex creates
-- the same index once AO is listening.
--
-- A migration runs before AO listens, and this build reads the whole archive
-- table, the largest in ao.db. With a cold file cache on a busy hard disk that
-- is minutes in which AO answers nothing; the launcher kills an AO that is not
-- ready within 15 minutes, a build cut off there rolls back, and the next start
-- begins it again. On 2026-11-16 the first start after the update that brought
-- migration 9009's index (a 4.7 GB ao.db) was not ready in those 15 minutes and
-- was killed; that index build is the likeliest cause.
--
-- An install that ran this migration while it still built the index keeps it:
-- EnsureProviderChunkIndex finds it there and builds nothing. The version stays
-- in the ledger either way (TestMigrationVersionLedger).
SELECT 1;

-- +goose Down
DROP INDEX IF EXISTS idx_conversation_provider_events_chunks;
