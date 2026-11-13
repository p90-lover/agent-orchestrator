package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// replayRowsFixture builds a conversation that exercises every row filter of the
// snapshot reads: an edit branch whose parent rows past the fork point are out of
// the lineage, a rolled-back turn, a cancelled turn and a promoted turn (whose
// messages leave the snapshot but whose activity stays). Every message carries
// delivery content and every activity carries command output and streamed text
// large enough to spill into overflow pages.
func replayRowsFixture(t *testing.T) (*sqlite.Store, *sql.DB, domain.ConversationRecord) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	seedProject(t, s, "replay")
	rec := sampleRecord("replay")
	rec.Mode = domain.SessionModeChat
	rec.Metadata.ProviderConversationID = "thread-root"
	rec.Metadata.ControllerGeneration = "generation-root"
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, "conversation-replay", domain.ConversationScopeSession,
		"replay", session.ID, testNow)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	rootID := conversation.ActiveBranchID

	at := testNow
	turn := func(generation, suffix string) {
		t.Helper()
		at = at.Add(time.Minute)
		appendBranchPrompt(t, s, session, conversation, generation, suffix, strings.ToUpper(suffix))
		providerTurn := "provider-turn-" + suffix
		if err := s.BindTurnToProvider(ctx, "turn-"+suffix, providerTurn, at); err != nil {
			t.Fatalf("BindTurnToProvider %s: %v", suffix, err)
		}
		if err := s.UpsertActivity(ctx, conversation.ID, providerTurn, domain.ConversationActivity{
			ID: "act-" + suffix, Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusCompleted,
			Summary: "run " + suffix, Detail: []byte(`{"command":"echo ` + suffix + `"}`), ProviderItemID: "item-" + suffix,
		}, at); err != nil {
			t.Fatalf("UpsertActivity %s: %v", suffix, err)
		}
		if err := s.SettleAssistantMessage(ctx, conversation.ID, "assistant-"+suffix, providerTurn,
			"answer "+suffix, "message-assistant-"+suffix, at); err != nil {
			t.Fatalf("SettleAssistantMessage %s: %v", suffix, err)
		}
		if err := s.SettleTurn(ctx, conversation.ID, providerTurn, domain.TurnStateCompleted, "", at); err != nil {
			t.Fatalf("SettleTurn %s: %v", suffix, err)
		}
	}
	turn("generation-root", "a")
	turn("generation-root", "b") // rolled back below
	turn("generation-root", "c") // cancelled below
	turn("generation-root", "d") // promoted below
	conversation, err = s.ConversationForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("ConversationForSession: %v", err)
	}
	forkAfter := conversation.LatestSequence
	turn("generation-root", "e") // past the fork point: outside the edit branch's lineage

	child := domain.ConversationBranch{
		ID: "branch-child", ConversationID: conversation.ID, SessionID: session.ID,
		ProviderConversationID: "thread-child", ParentBranchID: rootID,
		ForkAfterTurnID: "turn-d", ReplacedTurnID: "turn-e", ForkAfterSequence: forkAfter,
	}
	if err := s.CreateConversationBranch(ctx, child, at.Add(time.Minute)); err != nil {
		t.Fatalf("CreateConversationBranch: %v", err)
	}
	activateTestBranch(t, s, session, conversation, child.ID, child.ProviderConversationID, "generation-child")
	turn("generation-child", "f")

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, update := range []struct {
		query string
		args  []any
	}{
		{`UPDATE conversation_turns SET rolled_back_at = ? WHERE id = 'turn-b'`, []any{at.Add(time.Minute)}},
		{`UPDATE conversation_turns SET state = 'cancelled' WHERE id = 'turn-c'`, nil},
		{`UPDATE conversation_turns SET promoted_to_turn_id = 'turn-a' WHERE id = 'turn-d'`, nil},
		{`UPDATE conversation_messages
		    SET delivery_content_json = '[{"type":"image","url":"data:image/png;base64,' || replace(hex(zeroblob(4096)), '0', 'A') || '"}]'`, nil},
		{`UPDATE conversation_activities
		    SET command_output = replace(hex(zeroblob(8192)), '0', 'o'), command_output_truncated = 1,
		        streamed_text = replace(hex(zeroblob(8192)), '0', 's'), streamed_text_truncated = 1`, nil},
	} {
		if _, err := raw.Exec(update.query, update.args...); err != nil {
			t.Fatalf("%s: %v", update.query, err)
		}
	}
	return s, raw, conversation
}

// The native-history import reconciles against exactly the rows the full
// snapshot returns. Only the columns it never reads may differ: they are what
// made the read outlast a resume's budget after a restart on a busy disk.
func TestLoadConversationReplayRowsMatchesSnapshotWithoutPayloads(t *testing.T) {
	ctx := context.Background()
	s, _, conversation := replayRowsFixture(t)

	snapshot, err := s.LoadConversationSnapshot(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationSnapshot: %v", err)
	}
	replay, err := s.LoadConversationReplayRows(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("LoadConversationReplayRows: %v", err)
	}

	if !reflect.DeepEqual(replay.Turns, snapshot.Turns) {
		t.Fatalf("replay turns differ from the snapshot:\n got %+v\nwant %+v", replay.Turns, snapshot.Turns)
	}
	wantMessages := make([]domain.ConversationMessage, 0, len(snapshot.Messages))
	for _, message := range snapshot.Messages {
		if message.DeliveryContentJSON == "" {
			t.Fatalf("fixture message %s has no delivery content to drop", message.ID)
		}
		message.DeliveryContentJSON = ""
		wantMessages = append(wantMessages, message)
	}
	if !reflect.DeepEqual(replay.Messages, wantMessages) {
		t.Fatalf("replay messages differ from the snapshot's:\n got %+v\nwant %+v", replay.Messages, wantMessages)
	}
	wantActivities := make([]domain.ConversationActivity, 0, len(snapshot.Activities))
	for _, activity := range snapshot.Activities {
		if activity.CommandOutput == "" || activity.StreamedText == "" {
			t.Fatalf("fixture activity %s has no output to drop", activity.ID)
		}
		activity.CommandOutput, activity.CommandOutputTruncated = "", false
		activity.StreamedText, activity.StreamedTextTruncated = "", false
		wantActivities = append(wantActivities, activity)
	}
	if !reflect.DeepEqual(replay.Activities, wantActivities) {
		t.Fatalf("replay activities differ from the snapshot's:\n got %+v\nwant %+v", replay.Activities, wantActivities)
	}

	// The comparisons above only mean something if the fixture hit every filter.
	assertMessageTexts(t, replay.Messages, []string{"A", "answer a", "F", "answer f"})
	var activityIDs []string
	for _, activity := range replay.Activities {
		activityIDs = append(activityIDs, activity.ID)
		if len(activity.Detail) == 0 {
			t.Fatalf("activity %s lost its detail, which the import fingerprints", activity.ID)
		}
	}
	if want := []string{"act-a", "act-d", "act-f"}; !reflect.DeepEqual(activityIDs, want) {
		t.Fatalf("replay activities = %v, want %v", activityIDs, want)
	}
}

func TestLoadConversationReplayRowsReportsMissingConversation(t *testing.T) {
	s, _, _ := seededChatConversation(t)
	_, err := s.LoadConversationReplayRows(context.Background(), "no-such-conversation")
	if !errors.Is(err, store.ErrConversationNotFound) {
		t.Fatalf("LoadConversationReplayRows error = %v, want ErrConversationNotFound", err)
	}
}

// branch_id is the last column of both tables. Matched from the row, it would
// make SQLite read every row to its end, overflow pages and all, so the
// projection would save nothing: the join must seek the (branch_id, sequence)
// index, and the select list must stop before the large columns.
func TestReplayRowsReadThroughTheBranchIndexes(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	for _, tc := range []struct {
		name, query, alias, index string
		dropped                   []string
	}{
		{"messages", store.ReplayMessagesSQL, "message", "idx_conversation_messages_branch", []string{"delivery_content_json"}},
		{"activities", store.ReplayActivitiesSQL, "activity", "idx_conversation_activities_branch", []string{"command_output", "streamed_text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, column := range tc.dropped {
				if strings.Contains(tc.query, column) {
					t.Fatalf("query reads %s, which the import never uses", column)
				}
			}
			rows, err := raw.Query("EXPLAIN QUERY PLAN "+tc.query, "conversation")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			seek := "SEARCH " + tc.alias + " USING INDEX " + tc.index + " (branch_id=?"
			found := false
			for _, step := range plan {
				if strings.HasPrefix(step, "SCAN "+tc.alias) {
					t.Fatalf("plan scans %s: %q", tc.alias, plan)
				}
				found = found || strings.HasPrefix(step, seek)
			}
			if !found {
				t.Fatalf("plan = %q; want a step starting %q", plan, seek)
			}
		})
	}
}
