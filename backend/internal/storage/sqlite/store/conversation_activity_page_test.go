package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func activityPageFixture(t *testing.T, activities int) (*sqlite.Store, *sql.DB, string) {
	t.Helper()
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	seedProject(t, s, "acts")
	session, err := s.CreateSession(ctx, sampleRecord("acts"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	conversation, err := s.CreateConversation(ctx, "conversation-acts", domain.ConversationScopeSession, "acts", session.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= activities; i++ {
		if err := s.UpsertActivity(ctx, conversation.ID, "", domain.ConversationActivity{
			ID: fmt.Sprintf("act-%d", i), Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted,
			Summary: fmt.Sprintf("activity %d", i), ProviderItemID: fmt.Sprintf("item-%d", i),
		}, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return s, raw, conversation.ID
}

func pageSummaries(t *testing.T, s *sqlite.Store, conversationID string) []string {
	t.Helper()
	page, err := s.LoadConversationSnapshotPage(context.Background(), conversationID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	return activitySummaries(page.Activities)
}

// A poll re-reads in full only the activities that changed: an unchanged
// revision means an unchanged row, so its large text is not read again.
func TestSnapshotPageReadsOnlyChangedActivitiesInFull(t *testing.T) {
	s, raw, conversationID := activityPageFixture(t, 2)
	if got := pageSummaries(t, s, conversationID); !reflect.DeepEqual(got, []string{"activity 1", "activity 2"}) {
		t.Fatalf("first page = %v", got)
	}
	// Changed behind the store's back without a revision bump: no store write
	// does this, so seeing the old text proves the row was not read again.
	if _, err := raw.Exec(`UPDATE conversation_activities SET summary = 'not read' WHERE provider_item_id = 'item-1'`); err != nil {
		t.Fatal(err)
	}
	if got := pageSummaries(t, s, conversationID); !reflect.DeepEqual(got, []string{"activity 1", "activity 2"}) {
		t.Fatalf("unchanged revision page = %v; want the remembered rows", got)
	}
	if _, err := raw.Exec(`UPDATE conversation_activities SET summary = 'activity 1 updated', revision = revision + 1 WHERE provider_item_id = 'item-1'`); err != nil {
		t.Fatal(err)
	}
	if got := pageSummaries(t, s, conversationID); !reflect.DeepEqual(got, []string{"activity 1 updated", "activity 2"}) {
		t.Fatalf("after a revision bump = %v; want the changed row read again", got)
	}
	if _, err := raw.Exec(`DELETE FROM conversation_activities WHERE provider_item_id = 'item-2'`); err != nil {
		t.Fatal(err)
	}
	if got := pageSummaries(t, s, conversationID); !reflect.DeepEqual(got, []string{"activity 1 updated"}) {
		t.Fatalf("after a delete = %v; a remembered row must not outlive its row", got)
	}
}

// activityPageKeysSQL mirrors the generated SelectConversationActivitiesPage;
// both must pick the same rows in the same order for every page, in a chat with
// one branch and in one with an edit branch, a rolled-back turn, a cancelled
// turn and a promoted turn.
func TestActivityPageKeysMatchTheGeneratedPageQuery(t *testing.T) {
	plain, plainRaw, plainID := activityPageFixture(t, 5)
	branched, branchedRaw, branchedConversation := replayRowsFixture(t)
	for _, fixture := range []struct {
		name           string
		s              *sqlite.Store
		raw            *sql.DB
		conversationID string
		newest         []string
	}{
		{"one branch", plain, plainRaw, plainID, []string{"act-5", "act-4", "act-3", "act-2", "act-1"}},
		{"edit branch and discarded turns", branched, branchedRaw, branchedConversation.ID, []string{"act-f", "act-d", "act-a"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := context.Background()
			var latest int64
			if err := fixture.raw.QueryRowContext(ctx, `SELECT latest_sequence FROM conversations WHERE id = ?`,
				fixture.conversationID).Scan(&latest); err != nil {
				t.Fatal(err)
			}
			for before := int64(1); before <= latest+1; before++ {
				for limit := int64(1); limit <= 6; limit++ {
					want, err := gen.New(fixture.raw).SelectConversationActivitiesPage(ctx, gen.SelectConversationActivitiesPageParams{
						ConversationID: fixture.conversationID, BeforeSequence: before, PageLimit: limit,
					})
					if err != nil {
						t.Fatal(err)
					}
					wantIDs := make([]string, 0, len(want))
					for _, row := range want {
						wantIDs = append(wantIDs, row.ID)
					}
					got, err := fixture.s.ActivityPageKeysForTest(ctx, fixture.conversationID, before, limit)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) == 0 && len(wantIDs) == 0 {
						continue
					}
					if !reflect.DeepEqual(got, wantIDs) {
						t.Fatalf("page before %d limit %d: keys %v, generated %v", before, limit, got, wantIDs)
					}
				}
			}
			// The pages above only prove something if the filters dropped rows.
			newest, err := fixture.s.ActivityPageKeysForTest(ctx, fixture.conversationID, latest+1, 50)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(newest, fixture.newest) {
				t.Fatalf("newest page = %v, want %v", newest, fixture.newest)
			}
		})
	}
}

// Every working chat card polls this read. branch_id is an activity row's last
// column, behind its command output and streamed text, so a plan that opens the
// rows reads each candidate's overflow chain; finding the discarded turns
// through the chat's turn index opened every turn the chat ever had. On
// 2026-11-16 the read was 46% of the daemon's samples with the disk 100% busy.
// The keys must come from indexes alone, newest first, without a sort.
func TestActivityPageKeysReadOnlyIndexes(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	rows, err := raw.Query("EXPLAIN QUERY PLAN "+store.ActivityPageKeysSQL, "conversation", int64(1<<40), int64(51))
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

	want := map[string]bool{
		"SEARCH activity USING COVERING INDEX idx_conversation_activities_page_keys (conversation_id=? AND sequence<?)":   false,
		"SEARCH on_path USING COVERING INDEX idx_conversation_activities_branch (branch_id=? AND sequence=? AND rowid=?)": false,
		"SEARCH discarded USING COVERING INDEX idx_conversation_turns_discarded (conversation_id=?)":                      false,
	}
	for _, step := range plan {
		if _, ok := want[step]; ok {
			want[step] = true
			continue
		}
		for _, table := range []string{"activity", "on_path", "discarded"} {
			if strings.Contains(step, " "+table+" ") {
				t.Errorf("plan %q reads %s rows at %q", plan, table, step)
			}
		}
		if strings.Contains(step, "TEMP B-TREE") || strings.Contains(step, "AUTOMATIC") {
			t.Errorf("plan %q sorts or builds an index at %q instead of walking one in order", plan, step)
		}
	}
	for step, found := range want {
		if !found {
			t.Errorf("plan %q has no step %q", plan, step)
		}
	}
}
