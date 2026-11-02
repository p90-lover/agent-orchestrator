package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
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
// both must pick the same rows in the same order for every page.
func TestActivityPageKeysMatchTheGeneratedPageQuery(t *testing.T) {
	s, raw, conversationID := activityPageFixture(t, 5)
	ctx := context.Background()
	for _, page := range []struct{ before, limit int64 }{{1 << 40, 3}, {1 << 40, 10}, {4, 2}, {2, 5}} {
		want, err := gen.New(raw).SelectConversationActivitiesPage(ctx, gen.SelectConversationActivitiesPageParams{
			ConversationID: conversationID, BeforeSequence: page.before, PageLimit: page.limit,
		})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs := make([]string, 0, len(want))
		for _, row := range want {
			wantIDs = append(wantIDs, row.ID)
		}
		got, err := s.ActivityPageKeysForTest(ctx, conversationID, page.before, page.limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 && len(wantIDs) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, wantIDs) {
			t.Fatalf("page before %d limit %d: keys %v, generated %v", page.before, page.limit, got, wantIDs)
		}
	}
}
