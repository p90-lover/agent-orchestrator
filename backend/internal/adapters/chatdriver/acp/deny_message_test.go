package acp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func newPermissionTestConversation() *conversation {
	return &conversation{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:       make(map[string]*parkedPermission),
		pendingInputs: make(map[string]*parkedInput),
		capabilities:  make(ports.ChatCapabilities),
		events:        make(chan ports.ChatEvent, 16),
		activeTurn:    "turn-1",
	}
}

// askPermission parks one Bash permission request and returns its AO request
// id and a channel with the ACP response the agent will receive.
func askPermission(t *testing.T, conv *conversation) (string, <-chan acpsdk.RequestPermissionResponse) {
	t.Helper()
	responses := make(chan acpsdk.RequestPermissionResponse, 1)
	go func() {
		response, err := conv.RequestPermission(context.Background(), acpsdk.RequestPermissionRequest{
			ToolCall: acpsdk.ToolCallUpdate{ToolCallId: "bash-1", Kind: acpsdk.Ptr(acpsdk.ToolKindExecute)},
			Options: []acpsdk.PermissionOption{
				{OptionId: "reject", Name: "Deny", Kind: acpsdk.PermissionOptionKindRejectOnce},
				{OptionId: "allow", Name: "Allow Once", Kind: acpsdk.PermissionOptionKindAllowOnce},
			},
		})
		if err != nil {
			t.Errorf("RequestPermission: %v", err)
		}
		responses <- response
	}()
	requested := nextEvent(t, conv.Events())
	if requested.Kind != ports.ChatEventApprovalRequested {
		t.Fatalf("first event = %#v, want an approval request", requested)
	}
	return requested.RequestID, responses
}

func deniedWith(t *testing.T, response acpsdk.RequestPermissionResponse) (string, string) {
	t.Helper()
	if response.Outcome.Selected == nil {
		t.Fatalf("permission response = %#v, want a selected option", response)
	}
	ao, _ := response.Meta[permissionMetaKey].(map[string]any)
	message, _ := ao["message"].(string)
	return string(response.Outcome.Selected.OptionId), message
}

func TestACPPermissionDenyCarriesReviewerReasonToTheAgent(t *testing.T) {
	conv := newPermissionTestConversation()
	requestID, responses := askPermission(t, conv)
	reason := "The command reviewer, not the user, denied this: it reads a file outside the project"
	if err := conv.ResolveRequest(context.Background(), requestID, ports.ChatDecision{ID: "reject", Message: reason}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	resolved := nextEvent(t, conv.Events())
	var detail map[string]string
	if err := json.Unmarshal(resolved.Detail, &detail); err != nil || detail["decision"] != "reject" || detail["message"] != reason {
		t.Fatalf("resolved detail = %s (%v), want the decision and its reason", resolved.Detail, err)
	}
	if option, message := deniedWith(t, <-responses); option != "reject" || message != reason {
		t.Fatalf("agent got option %q with message %q, want reject with the reviewer's reason", option, message)
	}
}

func TestACPPermissionAnswerWithoutMessageAddsNoMeta(t *testing.T) {
	conv := newPermissionTestConversation()
	requestID, responses := askPermission(t, conv)
	if err := conv.ResolveRequest(context.Background(), requestID, ports.ChatDecision{ID: "reject"}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	_ = nextEvent(t, conv.Events()) // approval.resolved
	response := <-responses
	if response.Meta != nil {
		t.Fatalf("permission response meta = %#v, want none for a person's plain answer", response.Meta)
	}
	if option, _ := deniedWith(t, response); option != "reject" {
		t.Fatalf("selected option = %q, want reject", option)
	}
}

// A decision the persistent host replays after AO restarts keeps its reason.
func TestACPPermissionReplayedDecisionKeepsItsReason(t *testing.T) {
	conv := newPermissionTestConversation()
	requestID, responses := askPermission(t, conv)
	reason := "Denied by Approve for me's safety rules"
	conv.applyPersistentInteraction(persistentInteractionCommand{
		EventID: "acp-host:approval", RequestID: requestID, Kind: persistentInteractionApproval,
		ProviderPending: true, Decision: &persistentDecision{ID: "reject", Message: reason},
	})
	_ = nextEvent(t, conv.Events()) // approval.resolved
	if option, message := deniedWith(t, <-responses); option != "reject" || message != reason {
		t.Fatalf("replayed answer = option %q, message %q", option, message)
	}
}
