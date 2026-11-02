package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// statementRecorder is a gen.DBTX that keeps the statement it is handed, so a
// test can EXPLAIN exactly what sqlc generated rather than a copy of it.
type statementRecorder struct {
	*sql.DB
	query string
}

func (r *statementRecorder) QueryRowContext(ctx context.Context, query string, _ ...any) *sql.Row {
	r.query = query
	return r.DB.QueryRowContext(ctx, "SELECT 0")
}

// Every new usage event runs CountUsageEventForNativeRootElsewhere inside the
// writer's transaction. With no index that finds a native root's bindings,
// SQLite walked the whole (binding_id, source_event_key) index of
// model_usage_events once per event: 40% of the daemon's page reads with six
// chats working, growing with the usage history. Both tables must be searched.
func TestUsageEventElsewhereProbeSearchesBothIndexes(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	recorder := &statementRecorder{DB: raw}
	params := gen.CountUsageEventForNativeRootElsewhereParams{
		SourceEventKey: "request-1",
		Harness:        domain.HarnessClaudeCode,
		NativeRootID:   "native-root",
		BindingID:      1,
	}
	if _, err := gen.New(recorder).CountUsageEventForNativeRootElsewhere(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query("EXPLAIN QUERY PLAN "+recorder.query,
		params.SourceEventKey, params.Harness, params.NativeRootID, params.BindingID)
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
	usesNativeRootIndex := false
	for _, step := range plan {
		if !strings.HasPrefix(step, "SEARCH ") {
			t.Fatalf("plan = %q; step %q is not an index search", plan, step)
		}
		usesNativeRootIndex = usesNativeRootIndex || strings.Contains(step, "idx_usage_bindings_native_root")
	}
	if !usesNativeRootIndex {
		t.Fatalf("plan = %q; want usage_bindings found through idx_usage_bindings_native_root", plan)
	}
}
