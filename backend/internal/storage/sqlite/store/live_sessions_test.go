package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestListLiveSessionsReturnsFullRecordsOfLiveSessionsOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	seedProject(t, s, "abc")
	for _, project := range []string{"mer", "abc", "mer"} {
		live := sampleRecord(project)
		live.Metadata.Prompt = "the full prompt " + project
		live.Metadata.RuntimeHandleID = "handle-" + project
		if _, err := s.CreateSession(ctx, live); err != nil {
			t.Fatal(err)
		}
		ended := sampleRecord(project)
		ended.IsTerminated = true
		if _, err := s.CreateSession(ctx, ended); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.ListAllSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.ListLiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, rec := range all {
		if !rec.IsTerminated {
			want = append(want, string(rec.ID))
		}
	}
	var got []string
	for _, rec := range live {
		got = append(got, string(rec.ID))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("live ids = %v, want %v in ListAllSessions order", got, want)
	}
	byID := map[string]any{}
	for _, rec := range all {
		byID[string(rec.ID)] = rec
	}
	for _, rec := range live {
		if !reflect.DeepEqual(any(rec), byID[string(rec.ID)]) {
			t.Fatalf("live record %s differs from ListAllSessions:\n got %+v\nwant %+v", rec.ID, rec, byID[string(rec.ID)])
		}
		if rec.Metadata.Prompt == "" || rec.Metadata.RuntimeHandleID == "" {
			t.Fatalf("live record %s is not a full record: %+v", rec.ID, rec)
		}
	}
}

// The reaper lists live sessions every five seconds. Scanning the table read
// every session ever created and its prompt overflow pages; the partial index
// finds the live rows alone, already in order.
func TestListLiveSessionsFindsLiveRowsThroughThePartialIndex(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	// The generated statement's select list does not change the plan; its
	// WHERE and ORDER BY are these.
	rows, err := raw.Query("EXPLAIN QUERY PLAN SELECT * FROM sessions WHERE is_terminated = 0 ORDER BY project_id, num")
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
	if len(plan) != 1 || !strings.Contains(plan[0], "USING INDEX idx_sessions_live_preview") {
		t.Fatalf("plan = %q; want one scan of sessions USING INDEX idx_sessions_live_preview, with no sort", plan)
	}
}
