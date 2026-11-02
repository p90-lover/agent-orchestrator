package sessionmanager

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A startup restore waits while a user is starting a chat, and goes on as soon
// as that spawn is done.
func TestStartupRestoreYieldsToUserSpawns(t *testing.T) {
	m := &Manager{}
	start := time.Now()
	m.waitForUserSpawns(context.Background(), time.Second)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("waited although no spawn was in flight")
	}

	m.userSpawns.Add(1)
	done := make(chan struct{})
	go func() { m.waitForUserSpawns(context.Background(), time.Minute); close(done) }()
	select {
	case <-done:
		t.Fatal("the restore went ahead while a user spawn was in flight")
	case <-time.After(400 * time.Millisecond):
	}
	m.userSpawns.Add(-1)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the restore did not resume after the spawn finished")
	}

	// A spawn that never ends cannot hold a restore past its limit.
	m.userSpawns.Add(1)
	start = time.Now()
	m.waitForUserSpawns(context.Background(), 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("waited %v; want about the 300ms limit", elapsed)
	}
}

// The restore yields before it takes the project's workspace gate: a user
// spawn in the same project needs that gate, so yielding while holding it
// stalled the spawn for the whole yield limit.
func TestStartupRestoreYieldsWithoutHoldingTheWorkspaceGate(t *testing.T) {
	m, st, rt, _ := newLifecycleManager()
	rec := domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		IsTerminated: true,
		Metadata:     domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "ao/mer-1/root", AgentSessionID: "agent-w"},
		Activity:     domain.Activity{State: domain.ActivityExited},
	}
	st.sessions["mer-1"] = rec
	st.worktrees["mer-1"] = []domain.SessionWorktreeRecord{
		{SessionID: "mer-1", RepoName: domain.RootWorkspaceRepoName, WorktreePath: "/ws/mer-1"},
	}

	m.userSpawns.Add(1)
	done := make(chan struct{})
	go func() { m.restoreAllSession(context.Background(), rec); close(done) }()
	time.Sleep(300 * time.Millisecond)
	if !workspaceGateFree(m, "mer") {
		t.Fatal("the restore holds the workspace gate while it yields to a user spawn")
	}
	m.userSpawns.Add(-1)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore did not resume after the spawn finished")
	}
	if rt.created != 1 {
		t.Fatalf("restored session relaunched %d times, want 1", rt.created)
	}
}
