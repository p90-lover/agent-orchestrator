package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// A turn's floor_sequence must be at or below every row the turn will ever
// have, or the lookups that start there miss rows: a reply left streaming after
// its turn ends is a spinner that never stops. Rows of a running turn arrive
// after later prompts are queued and while provider-started turns run, and a
// provider re-announces its turns on resume.
func TestTurnFloorIsBelowEveryRowOfItsTurn(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	seedProject(t, s, "turn-floor")
	record := sampleRecord("turn-floor")
	record.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, record)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, "conv-turn-floor", domain.ConversationScopeSession,
		"turn-floor", session.ID, histClock)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		t.Fatalf("claim controller generation: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	floorOf := func(turnID string) sql.NullInt64 {
		t.Helper()
		var floor sql.NullInt64
		if err := raw.QueryRowContext(ctx, `SELECT floor_sequence FROM conversation_turns WHERE id = ?`, turnID).Scan(&floor); err != nil {
			t.Fatalf("floor of %s: %v", turnID, err)
		}
		return floor
	}
	sequenceOf := func(messageID string) int64 {
		t.Helper()
		var sequence int64
		if err := raw.QueryRowContext(ctx, `SELECT sequence FROM conversation_messages WHERE id = ?`, messageID).Scan(&sequence); err != nil {
			t.Fatalf("sequence of %s: %v", messageID, err)
		}
		return sequence
	}
	streaming := func(messageID string) bool {
		t.Helper()
		var on int
		if err := raw.QueryRowContext(ctx, `SELECT streaming FROM conversation_messages WHERE id = ?`, messageID).Scan(&on); err != nil {
			t.Fatalf("streaming of %s: %v", messageID, err)
		}
		return on == 1
	}
	prompt := func(turnID string, at time.Duration) {
		t.Helper()
		created, err := s.AppendUserMessage(ctx, conversation.ID, session.ID, "gen-1", domain.ConversationMessage{
			ID: turnID + "-prompt", Text: turnID, Origin: domain.MessageOriginHuman, ClientMessageID: turnID + "-client",
		}, turnID, histClock.Add(at))
		if err != nil || !created {
			t.Fatalf("append %s: created=%v err=%v", turnID, created, err)
		}
	}
	reply := func(itemID, providerTurnID string) {
		t.Helper()
		if err := s.AppendAssistantDelta(ctx, conversation.ID, itemID, providerTurnID, "partial", itemID+"-message", histClock); err != nil {
			t.Fatalf("reply %s: %v", itemID, err)
		}
	}

	// A human turn's floor is its prompt's own sequence.
	prompt("running", 0)
	if floor := floorOf("running"); !floor.Valid || floor.Int64 != sequenceOf("running-prompt") {
		t.Fatalf("floor of running = %+v, want the prompt's sequence %d", floor, sequenceOf("running-prompt"))
	}
	if err := s.BindTurnToProvider(ctx, "running", "provider-running", histClock); err != nil {
		t.Fatalf("bind running: %v", err)
	}
	reply("item-1", "provider-running")
	// A follow-up queued while the agent works, then more of the running reply.
	prompt("queued", time.Second)
	reply("item-2", "provider-running")
	// A turn the provider starts on its own: its floor is the chat's sequence at
	// that moment, and a re-announcement on resume keeps it.
	if err := s.AdoptProviderTurn(ctx, conversation.ID, session.ID, "gen-1", "adopted", "provider-adopted", histClock); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	adoptedFloor := floorOf("adopted")
	if !adoptedFloor.Valid || adoptedFloor.Int64 != sequenceOf("item-2-message") {
		t.Fatalf("floor of adopted = %+v, want the chat's latest sequence %d", adoptedFloor, sequenceOf("item-2-message"))
	}
	reply("item-3", "provider-adopted")
	reply("item-4", "provider-running")
	if err := s.AdoptProviderTurn(ctx, conversation.ID, session.ID, "gen-1", "adopted", "provider-adopted", histClock); err != nil {
		t.Fatalf("adopt again: %v", err)
	}
	if floor := floorOf("adopted"); floor != adoptedFloor {
		t.Fatalf("floor of re-announced turn = %+v, want %+v kept", floor, adoptedFloor)
	}
	for turnID, messages := range map[string][]string{
		"running": {"running-prompt", "item-1-message", "item-2-message", "item-4-message"},
		"queued":  {"queued-prompt"},
		"adopted": {"item-3-message"},
	} {
		floor := floorOf(turnID)
		for _, id := range messages {
			if sequence := sequenceOf(id); sequence < floor.Int64 {
				t.Errorf("%s row %s has sequence %d below the turn floor %d", turnID, id, sequence, floor.Int64)
			}
		}
	}

	// The running turn ends without item/completed for its replies: every one of
	// them settles, the other turn's live reply does not.
	if err := s.SettleTurn(ctx, conversation.ID, "provider-running", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatalf("settle running: %v", err)
	}
	for _, id := range []string{"item-1-message", "item-2-message", "item-4-message"} {
		if streaming(id) {
			t.Errorf("%s still streaming after its turn completed", id)
		}
	}
	if !streaming("item-3-message") {
		t.Fatal("item-3-message settled with another turn")
	}

	// A turn written before migration 9011 has no floor and is read from the
	// chat's first message.
	if _, err := raw.ExecContext(ctx, `UPDATE conversation_turns SET floor_sequence = NULL WHERE id = 'adopted'`); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleTurn(ctx, conversation.ID, "provider-adopted", domain.TurnStateInterrupted, "", histClock); err != nil {
		t.Fatalf("settle adopted: %v", err)
	}
	if streaming("item-3-message") {
		t.Fatal("item-3-message still streaming after its floorless turn ended")
	}
}
