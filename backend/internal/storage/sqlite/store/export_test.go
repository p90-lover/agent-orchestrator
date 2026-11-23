package store

import (
	"context"
	"time"
)

// ProviderChunkIndexExistsForTest reports whether the provider-chunk janitor's
// index is in the schema.
func (s *Store) ProviderChunkIndexExistsForTest(ctx context.Context) (bool, error) {
	return s.providerChunkIndexExists(ctx)
}

// SetProviderChunkIndexHoldsForTest sets how long the next index build keeps
// the writer from a waiting write, and how long with none waiting.
func (s *Store) SetProviderChunkIndexHoldsForTest(hold, idle time.Duration) {
	s.chunkIndexMu.Lock()
	defer s.chunkIndexMu.Unlock()
	s.chunkIndexHold, s.chunkIndexIdleHold = hold, idle
}

// ProviderChunkIndexHoldForTest is the hold the next index build gets.
func (s *Store) ProviderChunkIndexHoldForTest() time.Duration {
	s.chunkIndexMu.Lock()
	defer s.chunkIndexMu.Unlock()
	return s.chunkIndexHold
}

// SetTurnFloorPaceForTest sets the floor backfill's read step and write batch,
// and drops its pauses.
func (s *Store) SetTurnFloorPaceForTest(readStep int64, writeBatch int) {
	s.turnFloorMu.Lock()
	defer s.turnFloorMu.Unlock()
	s.turnFloorPace = turnFloorPace{readStep: readStep, writeBatch: writeBatch}
}

// SetTurnFloorWriteHookForTest runs hook after each floor a write batch sets,
// inside the batch's transaction.
func (s *Store) SetTurnFloorWriteHookForTest(hook func(context.Context) error) {
	s.turnFloorMu.Lock()
	defer s.turnFloorMu.Unlock()
	s.turnFloorWriteHook = hook
}

// SetMaintenanceYieldForTest sets how long a background write keeps the writer
// once a write waits, and with none waiting.
func (s *Store) SetMaintenanceYieldForTest(yieldAfter, holdLimit time.Duration) {
	s.maintenanceYieldAfter, s.maintenanceHoldLimit = yieldAfter, holdLimit
}

// FloorlessTurnsIndexExistsForTest reports whether the floor backfill's index
// is in the schema.
func (s *Store) FloorlessTurnsIndexExistsForTest(ctx context.Context) (bool, error) {
	return s.indexExists(ctx, floorlessTurnsIndexName)
}

// Statements of the floor backfill, for the external tests' plan checks.
const (
	CountFloorlessTurnsSQL  = countFloorlessTurnsSQL
	FloorlessTurnChatsSQL   = floorlessTurnChatsSQL
	FloorlessTurnsOfChatSQL = floorlessTurnsOfChatSQL
	TurnFloorMessagesSQL    = turnFloorMessagesSQL
	TurnFloorActivitiesSQL  = turnFloorActivitiesSQL
	SetTurnFloorSQL         = setTurnFloorSQL
)

// ListPreviewSessionsSQL lets the external tests check the query plan of the
// exact statement the preview poller runs.
const ListPreviewSessionsSQL = listPreviewSessionsSQL

// ReplayMessagesSQL and ReplayActivitiesSQL let the external tests check the
// query plans of the native-history import's projected reads.
const (
	ReplayMessagesSQL   = replayMessagesSQL
	ReplayActivitiesSQL = replayActivitiesSQL
)

// ActivityPageKeysSQL lets the external tests check the query plan of the
// snapshot poll's activity key read.
const ActivityPageKeysSQL = activityPageKeysSQL

// PruneChunkCandidatesSQL and FirstChunkReceivedSinceSQL let the external tests
// check the query plans of the provider-chunk janitor's two reads.
const (
	PruneChunkCandidatesSQL    = pruneChunkCandidatesSQL
	FirstChunkReceivedSinceSQL = firstChunkReceivedSinceSQL
)

// ActivityPageKeysForTest runs activityPageKeysSQL, for comparing it with the
// generated SelectConversationActivitiesPage it mirrors.
func (s *Store) ActivityPageKeysForTest(ctx context.Context, conversationID string, beforeSequence, limit int64) ([]string, error) {
	rows, err := s.readDB.QueryContext(ctx, activityPageKeysSQL, conversationID, beforeSequence, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		var revision int64
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
