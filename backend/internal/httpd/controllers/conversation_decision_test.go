package controllers

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestApprovalDecisionTrimsAndBoundsItsMessage(t *testing.T) {
	plain := approvalDecision(ResolveConversationApprovalRequest{DecisionID: "reject"})
	if plain.ID != "reject" || plain.Message != "" || plain.Raw != nil {
		t.Fatalf("decision without a message = %#v", plain)
	}
	trimmed := approvalDecision(ResolveConversationApprovalRequest{DecisionID: "reject", Message: "  reads outside the project \n"})
	if trimmed.Message != "reads outside the project" {
		t.Fatalf("message = %q, want it trimmed", trimmed.Message)
	}
	// A multi-byte character straddling the bound is dropped, not split.
	long := approvalDecision(ResolveConversationApprovalRequest{
		DecisionID: "reject", Message: strings.Repeat("a", ports.MaxChatDecisionMessage-1) + "é and more",
	})
	if len(long.Message) > ports.MaxChatDecisionMessage || !utf8.ValidString(long.Message) {
		t.Fatalf("bounded message has %d bytes, valid UTF-8 %v", len(long.Message), utf8.ValidString(long.Message))
	}
}
