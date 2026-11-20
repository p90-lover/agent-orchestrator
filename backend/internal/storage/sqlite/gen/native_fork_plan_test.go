package gen_test

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Every snapshot page reads the native-fork floor. Before the message walk, the
// query must learn from idx_conversation_turns_provider alone whether the chat
// has any provider-backed turn (a chat without one used to have every message
// row opened, 2026-11-11). The walk itself must stay an in-order search of the
// chat's messages, so LIMIT 1 stops it at the first prompt that qualifies.
func TestNativeForkFloorAsksTheTurnIndexBeforeWalkingMessages(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	const name = "SelectConversationNativeForkAvailableAfterSequence"
	query, ok := generatedQueries(t)[name]
	if !ok {
		t.Fatalf("generated query %s not found", name)
	}
	plan := queryPlan(t, raw, name, query)

	guard := slices.Index(plan,
		"SEARCH provider_turn USING COVERING INDEX idx_conversation_turns_provider (conversation_id=?)")
	walk := slices.Index(plan,
		"SEARCH message USING INDEX idx_conversation_messages_order (conversation_id=? AND sequence>?)")
	if guard < 0 {
		t.Errorf("plan %q does not answer the provider-turn guard from idx_conversation_turns_provider alone", plan)
	}
	if walk < 0 {
		t.Errorf("plan %q does not walk the chat's messages in sequence order", plan)
	}
	if guard >= 0 && walk >= 0 && guard > walk {
		t.Errorf("plan %q codes the message walk before the provider-turn guard", plan)
	}
}
