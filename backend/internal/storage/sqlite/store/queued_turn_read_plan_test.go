package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// promptLookup is the plan step that finds a queued turn's prompt: an index
// ordered by (conversation_id, sequence), searched for this chat and walked in
// that order, so ORDER BY sequence DESC LIMIT 1 stops at the newest match.
var promptLookup = regexp.MustCompile(
	`^SEARCH prompt USING INDEX (idx_conversation_messages_order|sqlite_autoindex_conversation_messages_\d+) \(conversation_id=\?\)$`)

// After every completed turn a chat's controller reads the head of its send
// queue while it holds the dispatch lock, and a send to that chat waits for the
// answer. On 2026-11-13 that read joined conversation_messages on turn_id alone
// and walked the message table from the first row; a follow-up waited six
// minutes behind it and the chat lost its agent. Scoping the join by chat still
// read every message the chat ever had. The read must now cost what the queue
// holds: the queued turns through their own index, then the prompt found by a
// newest-first walk of the chat that stops at the first match.
func TestQueuedTurnReadsSearchTheQueueNotTheChatHistory(t *testing.T) {
	dataDir := t.TempDir()
	_ = sqlitetest.MustOpenAt(t, dataDir)
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()

	next := &statementRecorder{DB: raw}
	// The recorder answers every read with "SELECT 0", so the scan fails; only
	// the statement text matters here.
	_, _ = gen.New(next).SelectNextQueuedConversationTurn(ctx, "conversation")
	reserved := &statementRecorder{DB: raw}
	_, _ = gen.New(reserved).SelectReservedConversationTurnForPromotion(ctx,
		gen.SelectReservedConversationTurnForPromotionParams{ID: "turn", ConversationID: "conversation"})

	for _, read := range []struct {
		name, query string
		turnStep    *regexp.Regexp
	}{
		{"SelectNextQueuedConversationTurn", next.query,
			regexp.MustCompile(`^SEARCH conversation_turns USING INDEX idx_conversation_turns_queue_head \(conversation_id=\?\)$`)},
		{"SelectReservedConversationTurnForPromotion", reserved.query,
			regexp.MustCompile(`^SEARCH conversation_turns USING INDEX \w+ \(id=\?\)$`)},
	} {
		if read.query == "" {
			t.Fatalf("%s: no statement recorded", read.name)
		}
		plan := explainQueryPlan(t, raw, read.query)
		var turns, message, prompt bool
		for _, step := range plan {
			switch {
			case strings.HasPrefix(step, "SCAN "), strings.Contains(step, "AUTOMATIC"):
				t.Errorf("%s: plan %q reads a whole table at %q", read.name, plan, step)
			case strings.Contains(step, "TEMP B-TREE"):
				// A sort means every matching row is read before the first is known:
				// the queue head must come off its index in order, and the prompt
				// must be the first match of a newest-first walk.
				t.Errorf("%s: plan %q sorts at %q instead of reading an index in order", read.name, plan, step)
			}
			turns = turns || read.turnStep.MatchString(step)
			message = message || step == "SEARCH conversation_messages USING INTEGER PRIMARY KEY (rowid=?)"
			prompt = prompt || promptLookup.MatchString(step)
		}
		if !turns || !message || !prompt {
			t.Errorf("%s: plan %q; want the turn found by %s, its prompt found newest first in this chat (%s), "+
				"and the message read by rowid", read.name, plan, read.turnStep, promptLookup)
		}
	}
}

// A queued turn's prompt is found by walking the chat newest first, so later
// rows must not be mistaken for it: replies of the turn that was running when
// it was queued, and prompts queued after it.
func TestQueuedTurnReadsFindTheirOwnPromptBehindLaterMessages(t *testing.T) {
	dataDir := t.TempDir()
	s := sqlitetest.MustOpenAt(t, dataDir)
	ctx := context.Background()
	seedProject(t, s, "queue-read")
	record := sampleRecord("queue-read")
	record.Mode = domain.SessionModeChat
	session, err := s.CreateSession(ctx, record)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conversation, err := s.CreateConversation(ctx, "conv-queue-read", domain.ConversationScopeSession,
		"queue-read", session.ID, histClock)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := s.ClaimChatControllerGeneration(ctx, session.ID, "gen-1"); err != nil {
		t.Fatalf("claim controller generation: %v", err)
	}
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	appendPrompt := func(turnID, text string, at time.Duration) {
		t.Helper()
		created, err := s.AppendUserMessage(ctx, conversation.ID, session.ID, "gen-1", domain.ConversationMessage{
			ID: turnID + "-message", Text: text, Origin: domain.MessageOriginHuman,
			ClientMessageID: turnID + "-client", DeliveryContentJSON: `[{"type":"text","text":"` + text + `"}]`,
		}, turnID, histClock.Add(at))
		if err != nil || !created {
			t.Fatalf("append %s: created=%v err=%v", turnID, created, err)
		}
	}
	// A streamed reply, written the way the projection numbers it: the next
	// sequence of the chat's own counter.
	appendReply := func(id, turnID string) {
		t.Helper()
		var sequence int64
		if err := raw.QueryRowContext(ctx, `UPDATE conversations SET latest_sequence = latest_sequence + 1
WHERE id = ? RETURNING latest_sequence`, conversation.ID).Scan(&sequence); err != nil {
			t.Fatalf("allocate reply sequence: %v", err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO conversation_messages
    (id, conversation_id, turn_id, sequence, role, origin, text, created_at, updated_at)
VALUES (?, ?, ?, ?, 'assistant', 'provider', 'reply', ?, ?)`,
			id, conversation.ID, turnID, sequence, histClock, histClock); err != nil {
			t.Fatalf("append reply %s: %v", id, err)
		}
	}

	// The turn the agent was running when the follow-ups arrived.
	appendPrompt("running", "running prompt", 0)
	if _, err := raw.ExecContext(ctx,
		`UPDATE conversation_turns SET state = 'running', started_at = requested_at WHERE id = 'running'`); err != nil {
		t.Fatalf("start running turn: %v", err)
	}
	appendReply("reply-1", "running")
	appendPrompt("queued-1", "first follow-up", time.Second)
	appendReply("reply-2", "running")
	appendReply("reply-3", "running")
	appendPrompt("queued-2", "second follow-up", 2*time.Second)
	appendReply("reply-4", "running")

	head, err := s.NextQueuedTurn(ctx, conversation.ID)
	if err != nil {
		t.Fatalf("NextQueuedTurn: %v", err)
	}
	if head.TurnID != "queued-1" || head.Text != "first follow-up" || head.ClientMessageID != "queued-1-client" ||
		head.Origin != domain.MessageOriginHuman || !strings.Contains(head.DeliveryContentJSON, "first follow-up") {
		t.Fatalf("queue head = %+v, want queued-1 with its own prompt", head)
	}

	promoted, err := s.ReserveQueuedTurnForPromotion(ctx, conversation.ID, "queued-2", histClock.Add(time.Minute))
	if err != nil {
		t.Fatalf("reserve queued-2: %v", err)
	}
	if promoted.TurnID != "queued-2" || promoted.Text != "second follow-up" || promoted.ClientMessageID != "queued-2-client" {
		t.Fatalf("reserved = %+v, want queued-2 with its own prompt", promoted)
	}

	if err := s.SettleTurnByID(ctx, "queued-1", domain.TurnStateCompleted, "", histClock); err != nil {
		t.Fatalf("settle queued-1: %v", err)
	}
	if _, err := s.NextQueuedTurn(ctx, conversation.ID); err == nil {
		t.Fatal("NextQueuedTurn found a turn; want an empty queue while queued-2 is being promoted")
	}
}

func explainQueryPlan(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, make([]any, strings.Count(query, "?"))...)
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
