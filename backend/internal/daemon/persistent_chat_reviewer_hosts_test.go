package daemon

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A reviewer chat's host is named after its review, not after a session, so the
// session ids in the keep set never covered it and boot shut the host down right
// after recovery had restored it.
func TestKeepReviewerChatHostsCoversWhatBootRecoveryRestores(t *testing.T) {
	workspace := domain.SessionMetadata{WorkspacePath: "/ws"}
	records := []domain.SessionRecord{
		{ID: "worker-live", Mode: domain.SessionModeChat, Metadata: workspace},
		{ID: "worker-terminal", Mode: domain.SessionModeTUI, Metadata: workspace},
		{ID: "worker-terminated", Mode: domain.SessionModeChat, Metadata: workspace, IsTerminated: true},
		{ID: "worker-no-workspace", Mode: domain.SessionModeChat},
	}
	reviews := []domain.Review{
		{ID: "rev-1", SessionID: "worker-live"},
		{ID: "rev-2", SessionID: "worker-terminal"},
		{ID: "rev-3", SessionID: "worker-terminated"},
		{ID: "rev-4", SessionID: "worker-no-workspace"},
		{ID: "rev-5", SessionID: "worker-deleted"},
	}

	keep := persistentChatHostKeepSet(records)
	keepReviewerChatHosts(keep, records, reviews)

	for _, want := range []string{"review-rev-1", "review-rev-2"} {
		if _, ok := keep[want]; !ok {
			t.Fatalf("keep = %v, missing %s: its reviewer is recovered at boot", keep, want)
		}
	}
	for _, gone := range []string{"review-rev-3", "review-rev-4", "review-rev-5"} {
		if _, ok := keep[gone]; ok {
			t.Fatalf("keep = %v, has %s: its reviewer is not recovered", keep, gone)
		}
	}
}
