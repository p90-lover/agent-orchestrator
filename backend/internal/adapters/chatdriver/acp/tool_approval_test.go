package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func toolApprovalConversation(token string) *conversation {
	conv := newPermissionTestConversation()
	conv.toolApprovalToken = token
	conv.sessionID = "pi-session"
	return conv
}

func shellToolApproval(token string) ports.ChatToolApprovalRequest {
	return ports.ChatToolApprovalRequest{
		Token: token, ToolCallID: "call-1", ToolName: "bash", Kind: "execute", Title: "rm -rf build",
		Input: []byte(`{"command":"rm -rf build"}`),
	}
}

type toolApprovalAnswer struct {
	result ports.ChatToolApprovalResult
	err    error
}

func askToolApproval(conv *conversation, request ports.ChatToolApprovalRequest) <-chan toolApprovalAnswer {
	answers := make(chan toolApprovalAnswer, 1)
	go func() {
		result, err := conv.RequestToolApproval(context.Background(), request)
		answers <- toolApprovalAnswer{result, err}
	}()
	return answers
}

func TestToolApprovalParksLikeAPermissionRequestAndTakesTheChatsAnswer(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision ports.ChatDecision
		want     ports.ChatToolApprovalResult
	}{
		{name: "allowed", decision: ports.ChatDecision{ID: "allow"}, want: ports.ChatToolApprovalResult{Allowed: true}},
		{name: "denied", decision: ports.ChatDecision{ID: "reject"}},
		{
			name:     "denied by the reviewer",
			decision: ports.ChatDecision{ID: "reject", Message: "The reviewer denied this: it deletes the build"},
			want:     ports.ChatToolApprovalResult{Message: "The reviewer denied this: it deletes the build"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conv := toolApprovalConversation("secret")
			answers := askToolApproval(conv, shellToolApproval("secret"))
			requested := nextEvent(t, conv.Events())
			if requested.Kind != ports.ChatEventApprovalRequested || requested.Summary != "rm -rf build" ||
				requested.ActivityKind != domain.ActivityKindCommand || requested.ProviderTurnID != "turn-1" {
				t.Fatalf("event = %#v, want an approval request for the command in the open turn", requested)
			}
			var detail map[string]any
			if err := json.Unmarshal(requested.Detail, &detail); err != nil || detail["toolKind"] != "execute" {
				t.Fatalf("detail = %s (%v), want the tool kind", requested.Detail, err)
			}
			if input, _ := detail["input"].(map[string]any); input["command"] != "rm -rf build" {
				t.Fatalf("detail input = %#v, want the command for the card and the reviewer", detail["input"])
			}
			kinds := map[string]ports.ChatDecisionKind{}
			for _, option := range requested.Decisions {
				kinds[option.ID] = option.Kind
			}
			if kinds["allow"] != ports.ChatDecisionAllowOnce || kinds["reject"] != ports.ChatDecisionRejectOnce {
				t.Fatalf("decisions = %#v, want allow once and reject once", requested.Decisions)
			}
			if err := conv.ResolveRequest(context.Background(), requested.RequestID, test.decision); err != nil {
				t.Fatalf("ResolveRequest: %v", err)
			}
			answer := <-answers
			if answer.err != nil || answer.result != test.want {
				t.Fatalf("answer = %#v, %v; want %#v", answer.result, answer.err, test.want)
			}
		})
	}
}

// Under a persistent chat host an ACP request's answer is recorded with the
// host, which replays it to the agent after an AO restart. A tool approval's
// waiter lives in AO, so the host never saw it and is not asked: recording it
// there failed with "persistent interaction is not pending" (live, 2026-11-25).
func TestToolApprovalUnderAPersistentHostResolvesWithoutTheHost(t *testing.T) {
	conv := toolApprovalConversation("secret")
	conv.proc = &process{terminate: func() error { return nil }} // a host; conn is nil, so any call to it panics
	answers := askToolApproval(conv, shellToolApproval("secret"))
	requested := nextEvent(t, conv.Events())
	if err := conv.ResolveRequest(context.Background(), requested.RequestID, ports.ChatDecision{ID: "reject"}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	if resolved := nextEvent(t, conv.Events()); resolved.Kind != ports.ChatEventApprovalResolved {
		t.Fatalf("event = %#v, want the approval resolved", resolved)
	}
	if answer := <-answers; answer.err != nil || answer.result.Allowed {
		t.Fatalf("answer = %#v, %v; want denied", answer.result, answer.err)
	}
}

func TestToolApprovalGoesThroughTheBindingsPolicyFirst(t *testing.T) {
	conv := toolApprovalConversation("secret")
	conv.permissionMode = ports.PermissionModeBypassPermissions
	conv.permissionFor = func(mode ports.PermissionMode, params acpsdk.RequestPermissionRequest) (acpsdk.PermissionOptionId, bool) {
		if mode != ports.PermissionModeBypassPermissions || params.ToolCall.Kind == nil || *params.ToolCall.Kind != acpsdk.ToolKindExecute {
			t.Errorf("policy saw mode %q kind %v", mode, params.ToolCall.Kind)
		}
		return "allow", true
	}
	answer := <-askToolApproval(conv, shellToolApproval("secret"))
	if answer.err != nil || !answer.result.Allowed {
		t.Fatalf("answer = %#v, %v; want allowed by the policy", answer.result, answer.err)
	}
	select {
	case event := <-conv.Events():
		t.Fatalf("the policy's answer still reached the chat: %#v", event)
	default:
	}
}

func TestToolApprovalRefusesAWrongTokenAndABindingWithoutOne(t *testing.T) {
	if _, err := toolApprovalConversation("secret").RequestToolApproval(context.Background(), shellToolApproval("guess")); !errors.Is(err, ports.ErrChatToolApprovalToken) {
		t.Fatalf("wrong token: err = %v, want ErrChatToolApprovalToken", err)
	}
	if _, err := toolApprovalConversation("secret").RequestToolApproval(context.Background(), shellToolApproval("")); !errors.Is(err, ports.ErrChatToolApprovalToken) {
		t.Fatalf("no token: err = %v, want ErrChatToolApprovalToken", err)
	}
	if _, err := toolApprovalConversation("").RequestToolApproval(context.Background(), shellToolApproval("")); !errors.Is(err, ports.ErrChatUnsupported) {
		t.Fatalf("binding without tool approvals: err = %v, want ErrChatUnsupported", err)
	}
}
