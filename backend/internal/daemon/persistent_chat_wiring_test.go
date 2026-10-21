package daemon

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPersistentChatHostKeepSetUsesDurableOwnership(t *testing.T) {
	records := []domain.SessionRecord{
		{ID: "live-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex},
		{ID: "terminated-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, IsTerminated: true},
		{ID: "tui", Mode: domain.SessionModeTUI, Harness: domain.HarnessCodex},
		{ID: "other-provider", Mode: domain.SessionModeChat, Harness: domain.HarnessClaudeCode},
		// Boot does not relaunch an exited Chat, so its surviving host is an orphan.
		{ID: "exited-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessClaudeCode,
			Activity: domain.Activity{State: domain.ActivityExited}},
	}
	keep := persistentChatHostKeepSet(records)
	if len(keep) != 2 {
		t.Fatalf("keep = %v, want both live Chat providers", keep)
	}
	if _, ok := keep["live-chat"]; !ok {
		t.Fatalf("keep = %v, missing live-chat", keep)
	}
	if _, ok := keep["other-provider"]; !ok {
		t.Fatalf("keep = %v, missing other-provider", keep)
	}
}

func TestExitedChatHostsTakesChatsExitedPastTheGrace(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	exited := func(id domain.SessionID, mode domain.SessionMode, ago time.Duration) domain.SessionRecord {
		return domain.SessionRecord{ID: id, Mode: mode, Harness: domain.HarnessClaudeCode,
			Activity: domain.Activity{State: domain.ActivityExited, LastActivityAt: now.Add(-ago)}}
	}
	records := []domain.SessionRecord{
		exited("old-exit", domain.SessionModeChat, time.Hour),
		exited("just-exited", domain.SessionModeChat, 30*time.Second),
		exited("tui-exit", domain.SessionModeTUI, time.Hour),
		{ID: "no-exit-time", Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityExited}},
		{ID: "live", Mode: domain.SessionModeChat, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Hour)}},
	}
	got := exitedChatHosts(records, now)
	if len(got) != 1 || !got["old-exit"].Equal(now.Add(-time.Hour)) {
		t.Fatalf("exitedChatHosts = %v, want only old-exit at its exit time", got)
	}
}
