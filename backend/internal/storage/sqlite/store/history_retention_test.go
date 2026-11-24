package store_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

const day = 24 * time.Hour

// historyFixture is conversationFixture on a data dir the test can also open.
func historyFixture(t *testing.T) (*sqlite.Store, domain.SessionID, string, string) {
	t.Helper()
	dir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dir)
	ctx := context.Background()
	seedProject(t, s, "hist")
	rec := sampleRecord("hist")
	rec.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, "conv-1", domain.ConversationScopeSession, "hist", session.ID, histClock)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		t.Fatalf("claim controller generation: %v", err)
	}
	return s, session.ID, conversation.ID, dir
}

// The archive keeps a week. Older rows whose event id can never come again
// (empty, or a persistent host's) go; a history import's id keeps its row, now
// without payload, so loading that history again is still recognised as
// already applied. Rows a still-open turn may replay stay.
func TestSweepProviderEventArchiveRetiresOldRowsAndKeepsReplayKeys(t *testing.T) {
	s, session, conversation, _ := historyFixture(t)
	ctx := context.Background()
	noop := func(context.Context) error { return nil }
	now := histClock.Add(60 * day)
	archive := func(eventID, method string, at time.Time) bool {
		t.Helper()
		projected, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", eventID, method, `{"item":"`+strings.Repeat("p", 3000)+`"}`, at, noop)
		if err != nil {
			t.Fatalf("archive %s: %v", eventID, err)
		}
		return projected
	}
	archive("acp-host:aaaa:1:0", "activity.started", now.Add(-9*day))   // 0: old host event: deleted
	archive("", "approval.requested", now.Add(-9*day))                  // 1: old, no id: deleted
	archive("acp-history:abc", "activity.completed", now.Add(-8*day))   // 2: old history: emptied
	archive("acp-host:aaaa:2:0", "activity.completed", now.Add(-6*day)) // 3: younger than a week
	archive("acp-history:def", "activity.completed", now.Add(-time.Hour))
	events, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil || len(events) != 5 {
		t.Fatalf("archive = %d rows, %v; want 5", len(events), err)
	}
	id := func(i int) int64 { return events[i].ID }

	for _, check := range []struct {
		after  int64
		cutoff time.Time
		want   int64
	}{
		{0, now.Add(-7 * day), id(2)},
		{id(2), now.Add(-7 * day), id(2)},
		{0, now, id(4)},
		{0, now.Add(-30 * day), 0},
		{id(0), now.Add(-8*day - time.Minute), id(1)},
	} {
		got, err := s.ProviderEventIDReceivedBefore(ctx, check.after, check.cutoff)
		if err != nil || got != check.want {
			t.Fatalf("ProviderEventIDReceivedBefore(%d, %s) = %d, %v; want %d", check.after, check.cutoff, got, err, check.want)
		}
	}

	weekAgo := now.Add(-7 * day)
	sweep := func() (found, deleted, emptied int64) {
		t.Helper()
		found, deleted, emptied, last, err := s.SweepProviderEventArchive(ctx, weekAgo, 0, id(2), 100)
		if err != nil {
			t.Fatal(err)
		}
		if found > 0 && last != id(2) {
			t.Fatalf("last = %d, want %d", last, id(2))
		}
		return found, deleted, emptied
	}

	// A turn opened before these rows and still running could replay them.
	if err := s.AdoptProviderTurn(ctx, conversation, session, "gen-1", "turn-1", "provider-turn-1", now.Add(-10*day)); err != nil {
		t.Fatalf("adopt turn: %v", err)
	}
	if found, deleted, emptied := sweep(); found != 3 || deleted != 0 || emptied != 0 {
		t.Fatalf("with an older open turn: found %d deleted %d emptied %d; want 3, 0, 0", found, deleted, emptied)
	}
	if err := s.SettleTurn(ctx, conversation, "provider-turn-1", domain.TurnStateCompleted, "", now.Add(-5*day)); err != nil {
		t.Fatalf("settle turn: %v", err)
	}
	// A turn that began after them cannot.
	if err := s.AdoptProviderTurn(ctx, conversation, session, "gen-1", "turn-2", "provider-turn-2", now.Add(-time.Minute)); err != nil {
		t.Fatalf("adopt second turn: %v", err)
	}
	if found, deleted, emptied := sweep(); found != 3 || deleted != 2 || emptied != 1 {
		t.Fatalf("after the old turn ended: found %d deleted %d emptied %d; want 3, 2, 1", found, deleted, emptied)
	}
	if found, deleted, emptied := sweep(); found != 1 || deleted != 0 || emptied != 0 {
		t.Fatalf("again: found %d deleted %d emptied %d; want only the emptied row read, nothing written", found, deleted, emptied)
	}

	left, err := s.ProviderEventsSince(ctx, conversation, 0, 100)
	if err != nil || len(left) != 3 {
		t.Fatalf("remaining = %d rows, %v; want 3", len(left), err)
	}
	if left[0].ID != id(2) || left[0].ProviderEventID != "acp-history:abc" || left[0].PayloadJson != "" {
		t.Fatalf("history row = id %d %q payload %d bytes; want id %d, its event id, no payload", left[0].ID, left[0].ProviderEventID, len(left[0].PayloadJson), id(2))
	}
	if left[1].ID != id(3) || left[2].ID != id(4) || left[1].PayloadJson == "" || left[2].PayloadJson == "" {
		t.Fatal("rows younger than a week changed")
	}
	if archive("acp-history:abc", "activity.completed", now) {
		t.Fatal("a replayed history event was projected again after its row was emptied")
	}
}

// Tool cards older than 30 days keep at most 4 KB of each long text; younger
// and short cards stay byte for byte, nothing else on the card changes, and no
// change event fires (the trim leaves the revision alone).
func TestTrimActivityBodiesShortensOnlyOldLongTexts(t *testing.T) {
	s, _, conversation, _ := historyFixture(t)
	ctx := context.Background()
	now := histClock.Add(90 * day)
	long, nested := strings.Repeat("o", 10_000), strings.Repeat("é", 3_000) // 6,000 bytes of 2-byte runes
	detail, err := json.Marshal(map[string]any{
		"command": "go test ./...",
		"output":  long,
		"content": []any{map[string]any{"type": "text", "text": nested}, "short <&> text"},
		"exit":    json.Number("12345678901234567890"),
	})
	if err != nil {
		t.Fatal(err)
	}
	card := func(itemID string, created time.Time, detail []byte, output string) {
		t.Helper()
		if err := s.UpsertActivity(ctx, conversation, "", domain.ConversationActivity{
			ID: "activity-" + itemID, Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusCompleted,
			Summary: "go test ./...", Detail: detail, ProviderItemID: itemID,
		}, created); err != nil {
			t.Fatalf("seed %s: %v", itemID, err)
		}
		if output != "" {
			if found, err := s.AppendCommandOutput(ctx, conversation, itemID, output, created); err != nil || !found {
				t.Fatalf("output %s: %v %v", itemID, found, err)
			}
		}
	}
	card("old", now.Add(-40*day), detail, long)
	card("old-short", now.Add(-40*day), []byte(`{"command":"ls","output":"a.go"}`), "a.go\n")
	card("young", now.Add(-2*day), detail, long)

	before, err := s.LoadConversationSnapshot(ctx, conversation) // fills the activity row cache
	if err != nil || len(before.Activities) != 3 {
		t.Fatalf("snapshot = %d activities, %v; want 3", len(before.Activities), err)
	}
	head, err := s.LatestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-30 * day)
	through, err := s.ActivityRowIDCreatedBefore(ctx, 0, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	found, trimmed, _, err := s.TrimActivityBodies(ctx, cutoff, 0, through, 100, 4096)
	if err != nil || found != 1 || trimmed != 1 {
		t.Fatalf("trim = found %d trimmed %d, %v; want the one old long card", found, trimmed, err)
	}
	if again, err := s.LatestSeq(ctx); err != nil || again != head {
		t.Fatalf("change log head %d -> %d, %v: trimming old cards emitted change events", head, again, err)
	}

	after, err := s.LoadConversationSnapshot(ctx, conversation)
	if err != nil {
		t.Fatal(err)
	}
	items := func(activities []domain.ConversationActivity) map[string]domain.ConversationActivity {
		out := map[string]domain.ConversationActivity{}
		for _, a := range activities {
			out[a.ProviderItemID] = a
		}
		return out
	}
	was, byItem := items(before.Activities), items(after.Activities)
	old := byItem["old"]
	var got map[string]any
	dec := json.NewDecoder(strings.NewReader(string(old.Detail)))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("trimmed detail is not JSON: %v", err)
	}
	if got["output"] != long[:4096]+store.TrimmedTextNote {
		t.Fatalf("output = %d bytes; want the first 4 KB and the note", len(got["output"].(string)))
	}
	content := got["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, store.TrimmedTextNote) || strings.TrimSuffix(text, store.TrimmedTextNote) != nested[:4096] {
		t.Fatalf("nested text = %d bytes; want 4,096 bytes on a character boundary and the note", len(text))
	}
	if content[1] != "short <&> text" || got["command"] != "go test ./..." || got["exit"] != json.Number("12345678901234567890") {
		t.Fatalf("short values changed: %v %v %v", content[1], got["command"], got["exit"])
	}
	if old.CommandOutput != long[:4096]+store.TrimmedTextNote || !old.CommandOutputTruncated {
		t.Fatalf("command output = %d bytes truncated=%v; want 4 KB, the note, truncated", len(old.CommandOutput), old.CommandOutputTruncated)
	}
	if old.Revision != was["old"].Revision || old.Summary != "go test ./..." || old.Status != domain.ActivityStatusCompleted {
		t.Fatal("the card's revision, summary or status changed")
	}
	for _, item := range []string{"old-short", "young"} {
		if string(byItem[item].Detail) != string(was[item].Detail) || byItem[item].CommandOutput != was[item].CommandOutput || byItem[item].CommandOutputTruncated {
			t.Fatalf("%s card changed", item)
		}
	}

	if found, trimmed, _, err := s.TrimActivityBodies(ctx, cutoff, 0, through, 100, 4096); err != nil || trimmed != 0 || found != 1 {
		t.Fatalf("second trim = found %d trimmed %d, %v; want the old card read, nothing rewritten", found, trimmed, err)
	}
	// Even handed every row, the write re-checks the age: the young card stays.
	if _, trimmed, _, err := s.TrimActivityBodies(ctx, cutoff, 0, math.MaxInt64, 100, 4096); err != nil || trimmed != 0 {
		t.Fatalf("trim past the cutoff rewrote %d cards, %v; want none", trimmed, err)
	}
}

// Freed pages go back to the file system in steps, and only in incremental
// auto-vacuum mode (the launcher puts ao.db in it; a new test database is not).
func TestIncrementalVacuumStepGivesPagesBack(t *testing.T) {
	s, session, conversation, dir := historyFixture(t)
	ctx := context.Background()
	if _, _, incremental, err := s.FreePages(ctx); err != nil || incremental {
		t.Fatalf("a new test database reports incremental=%v, %v", incremental, err)
	}
	raw := rawDatabase(t, dir)
	if _, err := raw.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context) error { return nil }
	now := histClock.Add(30 * day)
	for i := range 200 {
		if _, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "", "activity.completed",
			`{"x":"`+strings.Repeat("x", 9000)+`"}`, now.Add(-10*day+time.Duration(i)*time.Second), noop); err != nil {
			t.Fatal(err)
		}
	}
	through, err := s.ProviderEventIDReceivedBefore(ctx, 0, now.Add(-7*day))
	if err != nil {
		t.Fatal(err)
	}
	if _, deleted, _, _, err := s.SweepProviderEventArchive(ctx, now.Add(-7*day), 0, through, 1000); err != nil || deleted != 200 {
		t.Fatalf("sweep deleted %d, %v; want 200", deleted, err)
	}
	free, pageSize, incremental, err := s.FreePages(ctx)
	if err != nil || !incremental || free < 400 || pageSize <= 0 {
		t.Fatalf("after the sweep: free %d page %d incremental %v, %v; want hundreds of free pages", free, pageSize, incremental, err)
	}
	pages := func() int64 {
		t.Helper()
		var n int64
		if err := raw.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := pages()
	freed, err := s.IncrementalVacuumStep(ctx, 100)
	if err != nil || freed != 100 {
		t.Fatalf("step freed %d, %v; want 100", freed, err)
	}
	if got := pages(); got != start-100 {
		t.Fatalf("page_count %d -> %d; want 100 fewer", start, got)
	}
	if left, _, _, err := s.FreePages(ctx); err != nil || left != free-100 {
		t.Fatalf("free pages %d -> %d, %v; want 100 fewer", free, left, err)
	}
}

func TestRetentionCursorOnlyMovesForward(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	ctx := context.Background()
	if got, err := s.RetentionCursor(ctx, "provider_archive"); err != nil || got != 0 {
		t.Fatalf("unset cursor = %d, %v; want 0", got, err)
	}
	for _, position := range []int64{10, 5} {
		if err := s.SetRetentionCursor(ctx, "provider_archive", position, histClock); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.RetentionCursor(ctx, "provider_archive"); err != nil || got != 10 {
		t.Fatalf("cursor = %d, %v; want 10 (never moved back)", got, err)
	}
	if got, err := s.RetentionCursor(ctx, "activity_cards"); err != nil || got != 0 {
		t.Fatalf("other cursor = %d, %v; want 0", got, err)
	}
}
