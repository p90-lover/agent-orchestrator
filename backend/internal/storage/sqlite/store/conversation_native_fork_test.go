package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// nativeForkChat creates a chat holding one human prompt per turn ID, at
// sequences 1..len(turnIDs). No turn is bound to a provider turn yet.
func nativeForkChat(t *testing.T, s *sqlite.Store, conversationID string, turnIDs ...string) {
	t.Helper()
	ctx := context.Background()
	rec := sampleRecord("native-fork")
	rec.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.CreateConversation(ctx, conversationID, domain.ConversationScopeSession,
		"native-fork", session.ID, testNow); err != nil {
		t.Fatalf("CreateConversation %s: %v", conversationID, err)
	}
	for i, turnID := range turnIDs {
		created, err := s.AppendUserMessage(ctx, conversationID, session.ID, "generation", domain.ConversationMessage{
			ID:     conversationID + "-" + turnID + "-prompt",
			Origin: domain.MessageOriginHuman,
			Text:   fmt.Sprintf("prompt %d", i+1),
		}, turnID, testNow.Add(time.Duration(i)*time.Minute))
		if err != nil || !created {
			t.Fatalf("AppendUserMessage %s: created=%v err=%v", turnID, created, err)
		}
	}
}

func nativeForkFloor(t *testing.T, s *sqlite.Store, conversationID string) int64 {
	t.Helper()
	page, err := s.LoadConversationSnapshotPage(context.Background(), conversationID, 0, 1)
	if err != nil {
		t.Fatalf("LoadConversationSnapshotPage %s: %v", conversationID, err)
	}
	return page.NativeForkAvailableAfterSequence
}

// The floor is the first human prompt whose turn the provider knows and has not
// rolled back. The turn-index guard in front of the message walk must not change
// that answer, only skip the walk when no turn of the chat can qualify.
func TestNativeForkFloorIsTheFirstProviderBackedPrompt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "native-fork")
	nativeForkChat(t, s, "fork-floor", "turn-1", "turn-2", "turn-3", "turn-4")

	if got := nativeForkFloor(t, s, "fork-floor"); got != 0 {
		t.Fatalf("floor with no provider-backed turn = %d, want 0", got)
	}
	if err := s.BindTurnToProvider(ctx, "turn-3", "provider-turn-3", testNow); err != nil {
		t.Fatalf("BindTurnToProvider turn-3: %v", err)
	}
	if got := nativeForkFloor(t, s, "fork-floor"); got != 3 {
		t.Fatalf("floor with only turn-3 provider-backed = %d, want its prompt at 3", got)
	}
	if err := s.BindTurnToProvider(ctx, "turn-1", "provider-turn-1", testNow); err != nil {
		t.Fatalf("BindTurnToProvider turn-1: %v", err)
	}
	if got := nativeForkFloor(t, s, "fork-floor"); got != 1 {
		t.Fatalf("floor with turn-1 provider-backed = %d, want 1", got)
	}
	// The provider-backed turns still exist, so the guard lets the walk run; the
	// walk must still find nothing once all of them are rolled back.
	if _, err := s.RollbackTurns(ctx, "fork-floor", "turn-1", testNow.Add(time.Hour)); err != nil {
		t.Fatalf("RollbackTurns: %v", err)
	}
	if got := nativeForkFloor(t, s, "fork-floor"); got != 0 {
		t.Fatalf("floor after rolling back every provider-backed turn = %d, want 0", got)
	}
}

// Every snapshot page reads the native-fork floor. In a chat whose turns never
// got a provider turn id, the walk used to open every message row the chat has
// before answering 0; right after a restart on 2026-11-11, 6 of AO's 8 readers
// were inside it on a cold disk. Such a chat must be answered without opening a
// single message row.
//
// A TEMP view shadows conversation_messages on one connection and fails on any
// row it yields. The bound chat proves the trap fires whenever the walk runs.
func TestNativeForkFloorOpensNoMessageOfAChatWithoutProviderTurns(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	seedProject(t, s, "native-fork")
	nativeForkChat(t, s, "never-bound", "plain-1", "plain-2", "plain-3")
	nativeForkChat(t, s, "bound", "bound-1")
	if err := s.BindTurnToProvider(ctx, "bound-1", "provider-bound-1", testNow); err != nil {
		t.Fatalf("BindTurnToProvider: %v", err)
	}

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	// TEMP objects are per connection.
	raw.SetMaxOpenConns(1)
	if _, err := raw.ExecContext(ctx, `
CREATE TEMP VIEW conversation_messages AS
SELECT * FROM main.conversation_messages
WHERE json(CASE WHEN sequence IS NOT NULL THEN 'opened a message row' END) IS NOT NULL`); err != nil {
		t.Fatalf("create message trap: %v", err)
	}
	q := gen.New(raw)

	got, err := q.SelectConversationNativeForkAvailableAfterSequence(ctx, "never-bound")
	if err != nil {
		t.Fatalf("floor of a chat without provider turns opened a message row: %v", err)
	}
	if got != 0 {
		t.Fatalf("floor of a chat without provider turns = %d, want 0", got)
	}
	if _, err := q.SelectConversationNativeForkAvailableAfterSequence(ctx, "bound"); err == nil ||
		!strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("floor of a bound chat err = %v, want the message trap to fire", err)
	}
}
