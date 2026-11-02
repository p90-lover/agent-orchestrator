package store_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The preview poller runs this four times a second. Answered from the table it
// read every sessions page plus the prompt overflow of each live row; the
// covering partial index answers it from a few index pages, with no sort.
func TestListPreviewSessionsIsAnsweredFromItsIndexAlone(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	rows, err := raw.Query("EXPLAIN QUERY PLAN " + store.ListPreviewSessionsSQL)
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
	if len(plan) != 1 || !strings.Contains(plan[0], "COVERING INDEX idx_sessions_live_preview") {
		t.Fatalf("plan = %q; want one scan of COVERING INDEX idx_sessions_live_preview", plan)
	}
}
