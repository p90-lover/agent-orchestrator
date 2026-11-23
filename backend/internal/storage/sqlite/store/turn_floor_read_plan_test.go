package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

var errStatementRecorded = errors.New("statement recorded, not run")

// turnFloorRecorder captures the statement and arguments a generated method
// sends, whichever of Exec, Query or QueryRow it uses.
type turnFloorRecorder struct {
	*sql.DB
	query string
	args  []any
}

func (r *turnFloorRecorder) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	r.query, r.args = query, args
	return nil, errStatementRecorded
}

func (r *turnFloorRecorder) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	r.query, r.args = query, args
	return nil, errStatementRecorded
}

func (r *turnFloorRecorder) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	r.query, r.args = query, args
	return r.DB.QueryRowContext(ctx, "SELECT 0 WHERE 0")
}

// boundedTurnRows is the only way a per-turn lookup may read a chat's message
// or activity rows: the chat's sequence index, entered at a lower bound (the
// turn's floor_sequence) and, for the lineage check, left at the fork.
var boundedTurnRows = regexp.MustCompile(
	`^SEARCH \w+ USING (COVERING )?INDEX \w+ \(conversation_id=\? AND sequence>\?( AND sequence<\?)?\)$`)

// No index on conversation_messages leads with turn_id, and building one reads
// the whole table. Every lookup of one turn's rows used to walk its chat's
// (conversation_id, sequence) index from the first message: SettleStreaming...
// does so on every completed turn inside the projection's writer transaction,
// 9,096 page reads (37 MB) in a 12,000-message chat. The lineage check of a
// forked chat scanned the messages of every chat, once per turn before the fork.
// Each of them must now start at the turn's floor_sequence.
func TestPerTurnLookupsStartAtTheTurnFloor(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()
	turn := sql.NullString{String: "turn", Valid: true}

	for _, lookup := range []struct {
		name    string
		call    func(*gen.Queries)
		aliases []string
	}{
		{"SettleStreamingConversationMessagesForTurn", func(q *gen.Queries) {
			_ = q.SettleStreamingConversationMessagesForTurn(ctx, gen.SettleStreamingConversationMessagesForTurnParams{
				ConversationID: "conversation", TurnID: turn})
		}, []string{"conversation_messages"}},
		{"SelectQueuedConversationMessage", func(q *gen.Queries) {
			_, _ = q.SelectQueuedConversationMessage(ctx, gen.SelectQueuedConversationMessageParams{
				ConversationID: "conversation", TurnID: turn})
		}, []string{"conversation_messages"}},
		{"UpdateQueuedConversationMessageText", func(q *gen.Queries) {
			_, _ = q.UpdateQueuedConversationMessageText(ctx, gen.UpdateQueuedConversationMessageTextParams{
				ConversationID: "conversation", TurnID: turn})
		}, []string{"conversation_messages"}},
		{"SelectRetryableConversationPrompt", func(q *gen.Queries) {
			_, _ = q.SelectRetryableConversationPrompt(ctx, gen.SelectRetryableConversationPromptParams{
				ID: "turn", ConversationID: "conversation"})
		}, []string{"conversation_messages"}},
		{"SelectConversationEditAnchor", func(q *gen.Queries) {
			_, _ = q.SelectConversationEditAnchor(ctx, gen.SelectConversationEditAnchorParams{
				ConversationID: "conversation", ReplacedTurnID: turn})
		}, []string{"message"}},
		{"SelectConversationUserMessageByTurn", func(q *gen.Queries) {
			_, _ = q.SelectConversationUserMessageByTurn(ctx, gen.SelectConversationUserMessageByTurnParams{
				ConversationID: "conversation", ProviderTurnID: "provider-turn"})
		}, []string{"conversation_messages"}},
		{"SelectConversationTurns", func(q *gen.Queries) {
			_, _ = q.SelectConversationTurns(ctx, "conversation")
		}, []string{"lineage_message", "lineage_activity"}},
		{"SelectConversationTurnsPage", func(q *gen.Queries) {
			_, _ = q.SelectConversationTurnsPage(ctx, gen.SelectConversationTurnsPageParams{
				ConversationID: "conversation", OldestSequence: 1, BeforeSequence: 100})
		}, []string{"lineage_message", "lineage_activity"}},
		{"ListVisibleRunningTurnsForConversation", func(q *gen.Queries) {
			_, _ = q.ListVisibleRunningTurnsForConversation(ctx, "conversation")
		}, []string{"lineage_message", "lineage_activity"}},
	} {
		recorder := &turnFloorRecorder{DB: raw}
		lookup.call(gen.New(recorder))
		if recorder.query == "" {
			t.Fatalf("%s: no statement recorded", lookup.name)
		}
		plan := explainTurnFloorPlan(t, raw, recorder.query, recorder.args)
		bounded := map[string]bool{}
		for _, step := range plan {
			for _, alias := range lookup.aliases {
				if !strings.HasPrefix(step, "SEARCH "+alias+" ") && !strings.HasPrefix(step, "SCAN "+alias) {
					continue
				}
				if !boundedTurnRows.MatchString(step) {
					t.Errorf("%s: plan %q reads %s at %q; want its chat's sequence index from the turn floor (%s)",
						lookup.name, plan, alias, step, boundedTurnRows)
				}
				bounded[alias] = true
			}
		}
		for _, alias := range lookup.aliases {
			if !bounded[alias] {
				t.Errorf("%s: plan %q never reads %s", lookup.name, plan, alias)
			}
		}
	}
}

func explainTurnFloorPlan(t *testing.T, db *sql.DB, query string, args []any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
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
	return plan
}
