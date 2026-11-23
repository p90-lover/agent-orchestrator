package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// floorChat is one chat of the backfill fixture.
type floorChat struct {
	s            *sqlite.Store
	raw          *sql.DB
	session      domain.SessionID
	conversation string
}

func newFloorChat(t *testing.T, s *sqlite.Store, raw *sql.DB, name string) floorChat {
	t.Helper()
	ctx := context.Background()
	record := sampleRecord("turn-floor")
	record.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, record)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, name, domain.ConversationScopeSession, "turn-floor", session.ID, histClock)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		t.Fatalf("claim controller generation: %v", err)
	}
	return floorChat{s: s, raw: raw, session: session.ID, conversation: conversation.ID}
}

func (c floorChat) prompt(t *testing.T, turnID string) {
	t.Helper()
	created, err := c.s.AppendUserMessage(context.Background(), c.conversation, c.session, "gen-1", domain.ConversationMessage{
		ID: turnID + "-prompt", Text: turnID, Origin: domain.MessageOriginHuman, ClientMessageID: turnID + "-client",
	}, turnID, histClock)
	if err != nil || !created {
		t.Fatalf("append %s: created=%v err=%v", turnID, created, err)
	}
}

func (c floorChat) bind(t *testing.T, turnID string) {
	t.Helper()
	if err := c.s.BindTurnToProvider(context.Background(), turnID, "provider-"+turnID, histClock); err != nil {
		t.Fatalf("bind %s: %v", turnID, err)
	}
}

func (c floorChat) adopt(t *testing.T, turnID string) {
	t.Helper()
	if err := c.s.AdoptProviderTurn(context.Background(), c.conversation, c.session, "gen-1", turnID, "provider-"+turnID, histClock); err != nil {
		t.Fatalf("adopt %s: %v", turnID, err)
	}
}

// reply streams an assistant reply into the turn; it stays streaming until the
// turn settles.
func (c floorChat) reply(t *testing.T, itemID, turnID string) {
	t.Helper()
	if err := c.s.AppendAssistantDelta(context.Background(), c.conversation, itemID, "provider-"+turnID, "partial", itemID+"-message", histClock); err != nil {
		t.Fatalf("reply %s: %v", itemID, err)
	}
}

// activity writes an activity row the way the store does: its sequence is the
// next one from the chat's counter. turnID "" writes one with no turn.
func (c floorChat) activity(t *testing.T, id, turnID string) {
	t.Helper()
	tx, err := c.raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var sequence int64
	if err := tx.QueryRow(`UPDATE conversations SET latest_sequence = latest_sequence + 1 WHERE id = ? RETURNING latest_sequence`,
		c.conversation).Scan(&sequence); err != nil {
		t.Fatalf("next sequence: %v", err)
	}
	turn := sql.NullString{String: turnID, Valid: turnID != ""}
	if _, err := tx.Exec(`INSERT INTO conversation_activities (id, conversation_id, turn_id, sequence, kind, status, created_at, updated_at)
VALUES (?, ?, ?, ?, 'command', 'completed', '2026-08-02 10:00:00', '2026-08-02 10:00:00')`, id, c.conversation, turn, sequence); err != nil {
		t.Fatalf("insert activity %s: %v", id, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (c floorChat) latest(t *testing.T) int64 {
	t.Helper()
	var latest int64
	if err := c.raw.QueryRow(`SELECT latest_sequence FROM conversations WHERE id = ?`, c.conversation).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	return latest
}

func (c floorChat) streaming(t *testing.T, itemID string) bool {
	t.Helper()
	var on int
	if err := c.raw.QueryRow(`SELECT streaming FROM conversation_messages WHERE id = ?`, itemID+"-message").Scan(&on); err != nil {
		t.Fatalf("streaming of %s: %v", itemID, err)
	}
	return on == 1
}

func floorsOf(t *testing.T, raw *sql.DB) map[string]sql.NullInt64 {
	t.Helper()
	rows, err := raw.Query(`SELECT id, floor_sequence FROM conversation_turns`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	floors := map[string]sql.NullInt64{}
	for rows.Next() {
		var id string
		var floor sql.NullInt64
		if err := rows.Scan(&id, &floor); err != nil {
			t.Fatal(err)
		}
		floors[id] = floor
	}
	return floors
}

// lowestRowOf is the lowest sequence among a turn's messages and activities,
// read the slow way.
func lowestRowOf(t *testing.T, raw *sql.DB, turnID string) sql.NullInt64 {
	t.Helper()
	var lowest sql.NullInt64
	if err := raw.QueryRow(`SELECT MIN(sequence) FROM (
    SELECT sequence FROM conversation_messages WHERE turn_id = ?1
    UNION ALL
    SELECT sequence FROM conversation_activities WHERE turn_id = ?1)`, turnID).Scan(&lowest); err != nil {
		t.Fatal(err)
	}
	return lowest
}

func visibleTurnIDs(t *testing.T, raw *sql.DB, conversationID string) []string {
	t.Helper()
	turns, err := gen.New(raw).SelectConversationTurns(context.Background(), conversationID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(turns))
	for _, turn := range turns {
		ids = append(ids, turn.ID)
	}
	return ids
}

func backfillTurnFloors(t *testing.T, s *sqlite.Store, logger *slog.Logger) bool {
	t.Helper()
	done, err := s.BackfillTurnFloors(context.Background(), logger)
	if err != nil {
		t.Fatalf("BackfillTurnFloors: %v", err)
	}
	return done
}

// Turns written before migration 9011 have no floor_sequence, and every lookup
// of their rows walks their chat from its first row: in a forked chat, once per
// such turn on every timeline page. The backfill gives each one its lowest
// row's sequence, exactly: a floor above any row of the turn would hide it from
// the lookups (a reply left streaming forever). A turn with no row yet gets
// the chat's latest sequence, below any row it can still get. The backfill runs
// after startup, never in a migration.
func TestTurnFloorBackfillGivesOldTurnsTheirLowestSequence(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	seedProject(t, s, "turn-floor")
	raw := rawDatabase(t, dataDir)
	chat := newFloorChat(t, s, raw, "conv-floors")
	other := newFloorChat(t, s, raw, "conv-other")

	// A turn whose replies arrive after a follow-up was queued, and an activity.
	chat.prompt(t, "running")
	chat.bind(t, "running")
	chat.reply(t, "item-1", "running")
	chat.prompt(t, "queued")
	chat.activity(t, "act-running", "running")
	chat.reply(t, "item-2", "running")
	// A provider-started turn whose first row is an activity.
	chat.adopt(t, "adopted")
	chat.activity(t, "act-adopted", "adopted")
	chat.reply(t, "item-3", "adopted")
	// Rows of no turn, and a provider-started turn with no row yet.
	chat.activity(t, "act-none", "")
	chat.adopt(t, "empty")
	other.prompt(t, "other-turn")
	other.bind(t, "other-turn")
	other.reply(t, "item-other", "other-turn")
	for i := 0; i < 5; i++ {
		other.activity(t, fmt.Sprintf("act-other-%d", i), "other-turn")
	}

	// Every turn so far was written "before migration 9011".
	if _, err := raw.Exec(`UPDATE conversation_turns SET floor_sequence = NULL`); err != nil {
		t.Fatal(err)
	}
	// A turn written after it keeps the floor its insert recorded.
	chat.prompt(t, "newer")
	newerFloor := floorsOf(t, raw)["newer"]
	if !newerFloor.Valid {
		t.Fatal("a new turn has no floor")
	}
	// Fork the chat after the adopted turn: the timeline's lineage check then
	// reads every turn before the fork.
	var root string
	if err := raw.QueryRow(`SELECT active_branch_id FROM conversations WHERE id = ?`, chat.conversation).Scan(&root); err != nil {
		t.Fatal(err)
	}
	var forkAfter int64
	if err := raw.QueryRow(`SELECT sequence FROM conversation_messages WHERE id = 'item-3-message'`).Scan(&forkAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO conversation_branches (id, conversation_id, session_id, provider_conversation_id, parent_branch_id, fork_after_sequence, created_at)
VALUES ('branch-edit', ?, ?, 'edit-thread', ?, ?, '2026-08-02 10:00:00')`, chat.conversation, string(chat.session), root, forkAfter); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE conversations SET active_branch_id = 'branch-edit' WHERE id = ?`, chat.conversation); err != nil {
		t.Fatal(err)
	}
	visibleBefore := visibleTurnIDs(t, raw, chat.conversation)
	latestBefore := chat.latest(t)

	exists, err := s.FloorlessTurnsIndexExistsForTest(ctx)
	if err != nil || exists {
		t.Fatalf("floorless turn index after Open = %v, %v; the migrations must not build it", exists, err)
	}
	if got := floorsOf(t, raw)["running"]; got.Valid {
		t.Fatalf("an old turn has floor %d before any backfill: Open must not fill floors", got.Int64)
	}

	// Two sequences per read and two turns per write, so the walk and the writes
	// both span several statements.
	s.SetTurnFloorPaceForTest(2, 2)
	var logs bytes.Buffer
	if done := backfillTurnFloors(t, s, slog.New(slog.NewTextHandler(&logs, nil))); !done {
		t.Fatal("BackfillTurnFloors left floorless turns")
	}
	if got := logs.String(); !strings.Contains(got, "floorless turn index built") || !strings.Contains(got, "chats=2") || !strings.Contains(got, "turns=5") {
		t.Fatalf("log = %q, want the index build and 5 turns of 2 chats reported", got)
	}

	floors := floorsOf(t, raw)
	for _, turnID := range []string{"running", "queued", "adopted", "other-turn"} {
		want := lowestRowOf(t, raw, turnID)
		if floors[turnID] != want {
			t.Errorf("floor of %s = %+v, want its lowest row's sequence %+v", turnID, floors[turnID], want)
		}
	}
	if got := floors["empty"]; !got.Valid || got.Int64 != latestBefore {
		t.Errorf("floor of the turn with no row = %+v, want the chat's latest sequence %d", got, latestBefore)
	}
	if got := floors["newer"]; got != newerFloor {
		t.Errorf("floor of a turn written after 9011 = %+v, want %+v kept", got, newerFloor)
	}
	if got := visibleTurnIDs(t, raw, chat.conversation); !slices.Equal(got, visibleBefore) {
		t.Errorf("forked timeline turns = %v after the backfill, want %v as before it", got, visibleBefore)
	}

	// The lookups find every row from the new floors.
	if err := s.SettleTurn(ctx, chat.conversation, "provider-running", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatalf("settle running: %v", err)
	}
	for _, item := range []string{"item-1", "item-2"} {
		if chat.streaming(t, item) {
			t.Errorf("%s still streaming after its backfilled turn completed", item)
		}
	}
	if !chat.streaming(t, "item-3") {
		t.Error("item-3 settled with another turn")
	}
	// The turn that had no row gets one, after a re-announcement.
	chat.adopt(t, "empty")
	chat.reply(t, "item-4", "empty")
	if err := s.SettleTurn(ctx, chat.conversation, "provider-empty", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatalf("settle empty: %v", err)
	}
	if chat.streaming(t, "item-4") {
		t.Error("the reply of a turn backfilled with no row still streams after its turn completed")
	}

	// Done is remembered in the schema: an empty index, one read on a new start.
	exists, err = s.FloorlessTurnsIndexExistsForTest(ctx)
	if err != nil || !exists {
		t.Fatalf("floorless turn index after the backfill = %v, %v", exists, err)
	}
	restarted, err := sqlite.OpenPreMigrated(dataDir)
	if err != nil {
		t.Fatalf("open the store again: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	logs.Reset()
	if done := backfillTurnFloors(t, restarted, slog.New(slog.NewTextHandler(&logs, nil))); !done || logs.Len() != 0 {
		t.Fatalf("a later start: done=%v log=%q; want done with nothing to do", done, logs.String())
	}
}

// Each floor write rewrites a turn row, and the writer is shared with every
// chat write. A batch still running when a chat write needs the writer is
// rolled back and tried again smaller; no floor is lost or wrong.
func TestTurnFloorBackfillGivesTheWriterToAWaitingWrite(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	seedProject(t, s, "turn-floor")
	raw := rawDatabase(t, dataDir)
	chat := newFloorChat(t, s, raw, "conv-yield")
	const turns = 30
	for i := 0; i < turns; i++ {
		turnID := fmt.Sprintf("turn-%02d", i)
		chat.prompt(t, turnID)
		chat.bind(t, turnID)
		chat.reply(t, "item-"+turnID, turnID)
	}
	if _, err := raw.Exec(`UPDATE conversation_turns SET floor_sequence = NULL`); err != nil {
		t.Fatal(err)
	}

	s.SetTurnFloorPaceForTest(1000, 20)
	s.SetMaintenanceYieldForTest(50*time.Millisecond, time.Hour)
	// The first batch stalls inside its transaction, as on a slow disk, until a
	// chat write waits behind it and the backfill gives the writer back.
	stalled := make(chan struct{})
	wrote := make(chan error, 1)
	var stalls int
	s.SetTurnFloorWriteHookForTest(func(ctx context.Context) error {
		if stalls > 0 {
			return nil
		}
		stalls++
		close(stalled)
		<-ctx.Done()
		return ctx.Err()
	})
	go func() {
		<-stalled
		wrote <- s.UpsertProject(ctx, domain.ProjectRecord{ID: "during", Path: "/tmp/during", RegisteredAt: time.Now().UTC().Truncate(time.Second)})
	}()
	yieldsBefore := s.MaintenanceYields()
	if done := backfillTurnFloors(t, s, nil); !done {
		t.Fatal("BackfillTurnFloors left floorless turns")
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("the waiting write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting write never got the writer")
	}
	if got := s.MaintenanceYields() - yieldsBefore; got < 1 {
		t.Fatalf("backfill yielded %d times, want the stalled batch given back", got)
	}
	floors := floorsOf(t, raw)
	for i := 0; i < turns; i++ {
		turnID := fmt.Sprintf("turn-%02d", i)
		if want := lowestRowOf(t, raw, turnID); floors[turnID] != want {
			t.Errorf("floor of %s = %+v, want %+v", turnID, floors[turnID], want)
		}
	}
}

// The backfill reads each chat through indexes that bound it to that chat, and
// lists its work from the index of floorless turns alone. EXPLAIN does not call
// that index covering, because the statements name floor_sequence; but the
// index's own WHERE implies that term, so SQLite does not evaluate it, and with
// no other column wanted it never seeks the turn row.
func TestTurnFloorBackfillReadsOneChatByIndex(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	if done := backfillTurnFloors(t, s, nil); !done {
		t.Fatal("an empty store has floorless turns")
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, statement := range []struct {
		name, query string
		args        []any
		want        *regexp.Regexp
	}{
		{"warm-up", store.CountFloorlessTurnsSQL, []any{0, 5000},
			regexp.MustCompile(`^SEARCH conversation_turns USING INTEGER PRIMARY KEY \(rowid>\? AND rowid<\?\)$`)},
		{"chats", store.FloorlessTurnChatsSQL, nil,
			regexp.MustCompile(`^SCAN conversation_turns USING (COVERING )?INDEX idx_conversation_turns_floorless$`)},
		{"turns of a chat", store.FloorlessTurnsOfChatSQL, []any{"c"},
			regexp.MustCompile(`^SEARCH conversation_turns USING (COVERING )?INDEX idx_conversation_turns_floorless \(conversation_id=\?\)$`)},
		{"messages", store.TurnFloorMessagesSQL, []any{"c", 0, 10},
			regexp.MustCompile(`^SEARCH conversation_messages USING INDEX \w+ \(conversation_id=\? AND sequence>\? AND sequence<\?\)$`)},
		{"activities", store.TurnFloorActivitiesSQL, []any{"c", 0, 10},
			regexp.MustCompile(`^SEARCH conversation_activities USING COVERING INDEX idx_conversation_activities_page_keys \(conversation_id=\? AND sequence>\? AND sequence<\?\)$`)},
		{"set floor", store.SetTurnFloorSQL, []any{1, "t", "c"},
			regexp.MustCompile(`^SEARCH conversation_turns USING INDEX sqlite_autoindex_\w+ \(id=\?\)$`)},
	} {
		plan := explainTurnFloorPlan(t, raw, statement.query, statement.args)
		matched := false
		for _, step := range plan {
			if statement.want.MatchString(step) {
				matched = true
			}
			if strings.HasPrefix(step, "SCAN conversation_messages") || strings.HasPrefix(step, "SCAN conversation_activities") ||
				(strings.HasPrefix(step, "SCAN conversation_turns") && !strings.Contains(step, "idx_conversation_turns_floorless")) {
				t.Errorf("%s: plan %q scans a whole table at %q", statement.name, plan, step)
			}
		}
		if !matched {
			t.Errorf("%s: plan %q, want a step matching %s", statement.name, plan, statement.want)
		}
	}
}
