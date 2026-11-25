package opencodeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/opencode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestLiveOpenCodeAsksTheChatBeforeItsShell drives a real `opencode acp`
// through AO's driver in accept edits (the chat's Ask for approval), against
// the stand-in Chat Completions API that asks for `git status`. It checks that
// the shell call reaches the chat as an approval request, that rejecting it
// keeps the command from running, and that a user's own deny rule still
// refuses the command without asking. It makes no model calls, but needs
// OpenCode 1.x, so it is skipped unless AO_OPENCODE_ASK_LIVE=1:
//
//	AO_OPENCODE_ASK_LIVE=1 AO_OPENCODE_ASK_HOME=<initialized xdg folder> \
//	  go test ./internal/adapters/chatdriver/opencodeacp/ -run LiveOpenCodeAsks -v
//
// OpenCode runs with its own folders in AO_OPENCODE_ASK_HOME (default: a temp
// folder, where it first installs its plugin package, which can take minutes).
func TestLiveOpenCodeAsksTheChatBeforeItsShell(t *testing.T) {
	if os.Getenv("AO_OPENCODE_ASK_LIVE") != "1" {
		t.Skip("set AO_OPENCODE_ASK_LIVE=1 to run against a real OpenCode")
	}
	home := os.Getenv("AO_OPENCODE_ASK_HOME")
	if home == "" {
		home = t.TempDir()
	}

	t.Run("rejected in the chat", func(t *testing.T) {
		model := newStandInChat(t)
		requests, details := runLiveAskTurn(t, home, model, nil, func(event ports.ChatEvent) ports.ChatDecision {
			for _, option := range event.Decisions {
				if option.Kind == ports.ChatDecisionRejectOnce {
					return ports.ChatDecision{ID: option.ID}
				}
			}
			t.Fatalf("the request offers no reject: %#v", event.Decisions)
			return ports.ChatDecision{}
		})
		t.Logf("activity:\n%s", strings.Join(details, "\n"))
		if len(requests) != 1 || !strings.Contains(requests[0], "git status") {
			t.Fatalf("approval requests = %q, want one for git status", requests)
		}
		output := model.toolOutput()
		t.Logf("tool output the model read:\n%s", output)
		if strings.Contains(output, "a.txt") || strings.Contains(output, "No commits yet") {
			t.Fatalf("git status ran although the chat rejected it:\n%s", output)
		}
	})

	t.Run("the user's own deny refuses without asking", func(t *testing.T) {
		model := newStandInChat(t)
		requests, details := runLiveAskTurn(t, home, model, map[string]any{"bash": map[string]string{"git *": "deny"}},
			func(event ports.ChatEvent) ports.ChatDecision {
				t.Errorf("asked about a command the user denied: %s", event.Detail)
				return ports.ChatDecision{ID: event.Decisions[0].ID}
			})
		t.Logf("activity:\n%s", strings.Join(details, "\n"))
		output := model.toolOutput()
		t.Logf("tool output the model read:\n%s", output)
		if len(requests) != 0 || strings.Contains(output, "a.txt") {
			t.Fatalf("the user's deny did not hold (requests %q):\n%s", requests, output)
		}
	})
}

// runLiveAskTurn sends one "run git status" turn in accept edits and answers
// each approval request with decide. It returns each request's detail and the
// turn's activity.
func runLiveAskTurn(t *testing.T, home string, model *standInChat, userPermission map[string]any,
	decide func(ports.ChatEvent) ports.ChatDecision) (requests, details []string) {
	t.Helper()
	overlay := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   "standin/m",
		"provider": map[string]any{"standin": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Stand-in",
			"options": map[string]any{"baseURL": model.URL + "/v1", "apiKey": "stand-in"},
			"models": map[string]any{"m": map[string]any{
				"name": "m", "tool_call": true, "limit": map[string]any{"context": 100000, "output": 1000},
			}},
		}},
	}
	if userPermission != nil {
		overlay["permission"] = userPermission
	}
	content, _ := json.Marshal(overlay)
	workspace := t.TempDir()
	seedGitWorkspace(t, workspace)

	driver := New(opencode.New(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	conversation, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "live-opencode-ask", DataDir: t.TempDir(), WorkspacePath: workspace,
		Model: "standin/m", Permissions: ports.PermissionModeAcceptEdits,
		Env: map[string]string{
			"OPENCODE_CONFIG_CONTENT": string(content),
			"XDG_CONFIG_HOME":         filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"),
			"XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_STATE_HOME": filepath.Join(home, "state"),
			"NO_PROXY": "127.0.0.1,localhost", "HTTPS_PROXY": "", "HTTP_PROXY": "", "ALL_PROXY": "",
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conversation.(ports.ChatProviderTerminator).Terminate()

	ref, err := conversation.SendTurn(ctx, ports.ChatUserMessage{
		Text: "run git status", ClientMessageID: "live-ask-1", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if starter, ok := conversation.(ports.ChatDeferredTurnStarter); ok {
		if err := starter.StartDeferredTurn(ref.ProviderTurnID); err != nil {
			t.Fatalf("StartDeferredTurn: %v", err)
		}
	}
	for {
		select {
		case event, ok := <-conversation.Events():
			if !ok {
				t.Fatal("event stream closed before the turn completed")
			}
			switch event.Kind {
			case ports.ChatEventApprovalRequested:
				requests = append(requests, fmt.Sprintf("%s %s %s %s", event.ActivityKind, event.Summary, event.Title, event.Detail))
				if err := conversation.ResolveRequest(ctx, event.RequestID, decide(event)); err != nil {
					t.Fatalf("ResolveRequest: %v", err)
				}
			case ports.ChatEventActivityStarted, ports.ChatEventActivityCompleted:
				details = append(details, fmt.Sprintf("%s %s %s", event.Kind, event.ActivityKind, event.Detail))
			case ports.ChatEventTurnCompleted:
				if acknowledger, ok := conversation.(ports.ChatProviderEventAcknowledger); ok {
					_ = acknowledger.AcknowledgeProviderEvent(context.Background(), event.ProviderEventID)
				}
				return requests, details
			case ports.ChatEventControllerState:
				if event.ControllerState == ports.ChatControllerStopped {
					t.Fatalf("controller stopped before the turn completed: %v", event.Err)
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out; requests %q, activity:\n%s", requests, strings.Join(details, "\n"))
		}
	}
}
