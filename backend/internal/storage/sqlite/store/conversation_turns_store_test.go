package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// A live reconnect reads only the turns. They must be the same turns the full
// snapshot shows, including the retry mark and the blanked provider turn id of a
// turn that belongs to an earlier provider scope; otherwise a reconnect could
// claim ownership of a turn the current provider never ran.
func TestLoadConversationTurnsMatchesSnapshotTurns(t *testing.T) {
	ctx := context.Background()
	s, session, conversation := seededChatConversation(t)
	rootID := conversation.ActiveBranchID
	appendBranchPrompt(t, s, session, conversation, "generation-root", "a", "A")
	if err := s.BindTurnToProvider(ctx, "turn-a", "provider-turn-a", testNow); err != nil {
		t.Fatalf("BindTurnToProvider turn-a: %v", err)
	}
	created, err := s.AppendRetryUserMessage(ctx, conversation.ID, session.ID, "generation-root",
		domain.ConversationMessage{
			ID: "message-a-retry", Origin: domain.MessageOriginHuman, Text: "A again",
			ClientMessageID: "client-a-retry",
		}, "turn-a-retry", "turn-a", testNow.Add(time.Second))
	if err != nil || !created {
		t.Fatalf("AppendRetryUserMessage: created=%v err=%v", created, err)
	}

	conversation, err = s.ConversationForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ConversationForSession before boundary: %v", err)
	}
	boundary := domain.ConversationBranch{
		ID: "provider-boundary", ConversationID: conversation.ID, SessionID: session.ID,
		ProviderConversationID: "thread-next", ParentBranchID: rootID,
		ForkAfterSequence: conversation.LatestSequence, ProviderScopeID: "provider-boundary",
	}
	if err := s.CreateConversationBranch(ctx, boundary, testNow.Add(time.Minute)); err != nil {
		t.Fatalf("CreateConversationBranch boundary: %v", err)
	}
	activateTestBranch(t, s, session, conversation, boundary.ID, boundary.ProviderConversationID, "generation-next")
	appendBranchPrompt(t, s, session, conversation, "generation-next", "c", "C")
	if err := s.BindTurnToProvider(ctx, "turn-c", "provider-turn-c", testNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("BindTurnToProvider turn-c: %v", err)
	}

	snapshot, err := s.LoadConversationSnapshot(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	turns, err := s.LoadConversationTurns(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationTurns: %v", err)
	}
	if !reflect.DeepEqual(turns, snapshot.Turns) {
		t.Fatalf("turns-only read differs from the snapshot:\n got %+v\nwant %+v", turns, snapshot.Turns)
	}

	// The comparison above only means something if the fixture exercised the
	// retry mark and the provider-scope filter.
	byID := make(map[string]domain.ConversationTurn, len(turns))
	for _, turn := range turns {
		byID[turn.ID] = turn
	}
	if len(byID) != 3 {
		t.Fatalf("turns = %+v, want turn-a, turn-a-retry and turn-c", turns)
	}
	if !byID["turn-a"].HasRetryAttempt {
		t.Fatalf("turn-a lost its retry mark: %+v", byID["turn-a"])
	}
	if got := byID["turn-a"].ProviderTurnID; got != "" {
		t.Fatalf("turn-a from the earlier provider scope kept provider turn id %q", got)
	}
	if got := byID["turn-c"]; got.ProviderTurnID != "provider-turn-c" || got.State != domain.TurnStateRunning {
		t.Fatalf("active-scope running turn = %+v, want provider-turn-c running", got)
	}
}

func TestLoadConversationTurnsReportsMissingConversation(t *testing.T) {
	s, _, _ := seededChatConversation(t)
	_, err := s.LoadConversationTurns(context.Background(), "no-such-conversation")
	if !errors.Is(err, store.ErrConversationNotFound) {
		t.Fatalf("LoadConversationTurns error = %v, want ErrConversationNotFound", err)
	}
}
