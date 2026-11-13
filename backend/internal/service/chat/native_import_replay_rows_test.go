package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const resumeImportCommandOutput = "On branch main\nnothing to commit, working tree clean\n"

// seedResumeImportConversation stores one settled turn from an earlier Chat
// interval: the prompt, the answer and a command with its output and streamed
// text, the columns a projected replay read leaves out.
func seedResumeImportConversation(t *testing.T, st *sqlite.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	existing, err := st.CreateConversation(ctx, "existing-conversation",
		domain.ConversationScopeSession, testProject, testSession, now)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.ClaimChatControllerGeneration(ctx, testSession, "old-generation"); err != nil {
		t.Fatalf("ClaimChatControllerGeneration: %v", err)
	}
	created, err := st.AppendUserMessage(ctx, existing.ID, testSession, "old-generation",
		domain.ConversationMessage{
			ID: "existing-user", Text: "What changed?", Origin: domain.MessageOriginHuman,
			ClientMessageID:     "original-chat-client-id",
			DeliveryContentJSON: `[{"type":"text","text":"What changed?"}]`,
		}, "existing-turn", now)
	if err != nil || !created {
		t.Fatalf("AppendUserMessage: created=%v err=%v", created, err)
	}
	if err := st.BindTurnToProvider(ctx, "existing-turn", "native-turn-1", now); err != nil {
		t.Fatalf("BindTurnToProvider: %v", err)
	}
	if err := st.SettleAssistantMessage(ctx, existing.ID,
		"native-answer-1", "native-turn-1", "Nothing is dirty.", "existing-answer", now); err != nil {
		t.Fatalf("SettleAssistantMessage: %v", err)
	}
	if err := st.UpsertActivity(ctx, existing.ID, "native-turn-1",
		domain.ConversationActivity{
			ID: "existing-command", Kind: domain.ActivityKindCommand, Status: domain.ActivityStatusCompleted,
			Summary: "Ran git status", Detail: json.RawMessage(`{"command":"git status"}`), ProviderItemID: "native-command-1",
		}, now); err != nil {
		t.Fatalf("UpsertActivity: %v", err)
	}
	if ok, err := st.AppendCommandOutput(ctx, existing.ID, "native-command-1", resumeImportCommandOutput, now); err != nil || !ok {
		t.Fatalf("AppendCommandOutput: ok=%v err=%v", ok, err)
	}
	if ok, err := st.AppendActivityStreamedText(ctx, existing.ID, "native-command-1", "$ git status\n", now); err != nil || !ok {
		t.Fatalf("AppendActivityStreamedText: ok=%v err=%v", ok, err)
	}
	if err := st.SettleTurn(ctx, existing.ID, "native-turn-1", domain.TurnStateCompleted, "", now); err != nil {
		t.Fatalf("SettleTurn: %v", err)
	}
}

// resumeImportReplay is the provider's native thread for that turn. Codex can
// rename item ids on replay, so only content identifies the known rows; the
// second command is new to AO.
func resumeImportReplay() []ports.ChatEvent {
	return []ports.ChatEvent{
		{Kind: ports.ChatEventTurnStarted, ProviderEventID: "history-start", ProviderTurnID: "native-turn-1"},
		{
			Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "history-user",
			ProviderTurnID: "native-turn-1", ProviderItemID: "history-item-1",
			ClientMessageID: "native-client-1", Text: "What changed?",
		},
		{
			Kind: ports.ChatEventActivityCompleted, ProviderEventID: "history-command",
			ProviderTurnID: "native-turn-1", ProviderItemID: "history-item-2",
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted,
			Summary: "Ran git status", Detail: json.RawMessage(`{"command":"git status"}`),
		},
		{
			Kind: ports.ChatEventActivityCompleted, ProviderEventID: "history-new-command",
			ProviderTurnID: "native-turn-1", ProviderItemID: "history-item-new-command",
			ActivityKind: domain.ActivityKindCommand, ActivityStatus: domain.ActivityStatusCompleted,
			Summary: "Ran git diff", Detail: json.RawMessage(`{"command":"git diff"}`),
		},
		{
			Kind: ports.ChatEventMessageCompleted, ProviderEventID: "history-answer",
			ProviderTurnID: "native-turn-1", ProviderItemID: "history-item-3", Text: "Nothing is dirty.",
		},
		{
			Kind: ports.ChatEventTurnCompleted, ProviderEventID: "history-complete",
			ProviderTurnID: "native-turn-1", TurnState: domain.TurnStateRecovered,
		},
	}
}

func resumeImportOptions(st *sqlite.Store, conv ports.ChatConversation, snapshotReads *atomic.Int32) chatsvc.Options {
	var idMu sync.Mutex
	nextID := 0
	return chatsvc.Options{
		Store: st, Sessions: st,
		Reader: chatsvc.SnapshotReaderFunc(func(ctx context.Context, conversationID string) (chatsvc.ConversationRows, error) {
			snapshotReads.Add(1)
			rows, err := st.LoadConversationSnapshot(ctx, conversationID)
			if err != nil {
				return chatsvc.ConversationRows{}, err
			}
			return chatsvc.ConversationRows{
				Conversation: rows.Conversation, Turns: rows.Turns,
				Messages: rows.Messages, Activities: rows.Activities,
			}, nil
		}),
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}},
		Log:     slog.New(slog.DiscardHandler),
		NewID: func() string {
			idMu.Lock()
			defer idMu.Unlock()
			nextID++
			return fmt.Sprintf("history-id-%d", nextID)
		},
	}
}

func resumeImportStart(t *testing.T) chatsvc.StartConfig {
	t.Helper()
	return chatsvc.StartConfig{
		SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
		WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", HistoryMode: ports.ChatHistoryRequired,
	}
}

// A resume after the provider process died imports the provider's native thread
// and reconciles it against AO's rows. With a ReplayReader that reconciliation
// never loads the full snapshot, and it deduplicates exactly as the full
// snapshot does although the projected rows carry no command output, streamed
// text or delivery content.
func TestResumeNativeHistoryImportReadsOnlyReplayRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay bool
	}{{"replay reader", true}, {"snapshot fallback", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := openStore(t)
			now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
			seedResumeImportConversation(t, st, now)
			conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: resumeImportReplay()}

			var snapshotReads, replayReads atomic.Int32
			opts := resumeImportOptions(st, conv, &snapshotReads)
			if tc.replay {
				opts.ReplayReader = chatsvc.NativeReplayReaderFunc(func(ctx context.Context, conversationID string) (chatsvc.ConversationRows, error) {
					replayReads.Add(1)
					rows, err := st.LoadConversationReplayRows(ctx, conversationID)
					if err != nil {
						return chatsvc.ConversationRows{}, err
					}
					for _, activity := range rows.Activities {
						if activity.CommandOutput != "" || activity.StreamedText != "" {
							t.Errorf("replay row %s carries output the import never reads", activity.ID)
						}
					}
					return chatsvc.ConversationRows{Turns: rows.Turns, Messages: rows.Messages, Activities: rows.Activities}, nil
				})
			}
			svc := chatsvc.New(opts)
			t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })

			ctrl, err := svc.Start(ctx, resumeImportStart(t))
			if err != nil {
				t.Fatalf("Start resume: %v", err)
			}
			if tc.replay {
				if got := snapshotReads.Load(); got != 0 {
					t.Fatalf("full snapshot reads during the resume = %d, want 0", got)
				}
				if got := replayReads.Load(); got != 1 {
					t.Fatalf("replay reads = %d, want 1", got)
				}
			} else if got := snapshotReads.Load(); got != 1 {
				t.Fatalf("fallback full snapshot reads = %d, want 1", got)
			}
			if got := conv.reads.Load(); got != 1 {
				t.Fatalf("native history reads = %d, want 1", got)
			}

			snapshot, err := st.LoadConversationSnapshot(ctx, ctrl.ConversationID())
			if err != nil {
				t.Fatalf("LoadConversationSnapshot: %v", err)
			}
			if len(snapshot.Messages) != 2 || snapshot.Messages[0].Text != "What changed?" || snapshot.Messages[1].Text != "Nothing is dirty." {
				t.Fatalf("imported messages = %#v; the replay must not duplicate known ones", snapshot.Messages)
			}
			if len(snapshot.Activities) != 2 || snapshot.Activities[0].Summary != "Ran git status" || snapshot.Activities[1].Summary != "Ran git diff" {
				t.Fatalf("imported activities = %#v; want the known command once and the new one", snapshot.Activities)
			}
			if got := snapshot.Activities[0].CommandOutput; got != resumeImportCommandOutput {
				t.Fatalf("known command output = %q; the import must leave the stored row alone", got)
			}
			if got := snapshot.Messages[0].DeliveryContentJSON; !strings.Contains(got, "What changed?") {
				t.Fatalf("stored delivery content = %q; the import must leave the stored row alone", got)
			}
			if len(snapshot.Turns) != 1 || snapshot.Turns[0].State != domain.TurnStateCompleted {
				t.Fatalf("imported turns = %#v", snapshot.Turns)
			}
		})
	}
}

// A failed replay read stops the resume the way a failed snapshot read did,
// before the provider's history is read or anything is projected.
func TestResumeNativeHistoryImportReplayReadFailureStopsTheResume(t *testing.T) {
	st := openStore(t)
	seedResumeImportConversation(t, st, time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC))
	conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: resumeImportReplay()}
	var snapshotReads atomic.Int32
	opts := resumeImportOptions(st, conv, &snapshotReads)
	readErr := errors.New("replay rows: disk busy")
	opts.ReplayReader = chatsvc.NativeReplayReaderFunc(func(context.Context, string) (chatsvc.ConversationRows, error) {
		return chatsvc.ConversationRows{}, readErr
	})
	svc := chatsvc.New(opts)
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })

	_, err := svc.Start(context.Background(), resumeImportStart(t))
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "load conversation before native history import") {
		t.Fatalf("Start error = %v, want the replay read failure", err)
	}
	if got := snapshotReads.Load(); got != 0 {
		t.Fatalf("full snapshot reads after a failed replay read = %d, want 0", got)
	}
	if got := conv.reads.Load(); got != 0 {
		t.Fatalf("native history reads after a failed replay read = %d, want 0", got)
	}
}
