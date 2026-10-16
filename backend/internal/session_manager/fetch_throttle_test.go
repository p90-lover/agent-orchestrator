package sessionmanager

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Back-to-back spawns must not each wait on the network for a default branch
// fetched moments ago; a fetch older than the interval runs again.
func TestDefaultBranchFetchIsThrottledPerRepo(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := &Manager{clock: func() time.Time { return now }}
	key := filepath.Clean("/repo") + "\x00refs/remotes/origin/main"

	if m.fetchedRecently(key) {
		t.Fatal("a repository never fetched counted as recently fetched")
	}
	m.recordFetch(key)
	now = now.Add(defaultBranchRefreshInterval - time.Second)
	if !m.fetchedRecently(key) {
		t.Fatal("a fetch within the interval was not reused")
	}
	if m.fetchedRecently(filepath.Clean("/other") + "\x00refs/remotes/origin/main") {
		t.Fatal("another repository shared the first one's fetch")
	}
	now = now.Add(2 * time.Second)
	if m.fetchedRecently(key) {
		t.Fatal("a fetch older than the interval was still reused")
	}
}

// A project whose sessions run in its folder skips the refresh entirely.
func TestFolderModeProjectSkipsTheDefaultBranchRefresh(t *testing.T) {
	m := &Manager{}
	project := domain.ProjectRecord{ID: "repo", Kind: domain.ProjectKindSingleRepo, Path: "/repo",
		Config: domain.ProjectConfig{WorkspaceMode: domain.WorkspaceModeFolder}}
	if refs := m.refreshDefaultBranchesBestEffort(t.Context(), project); refs != nil {
		t.Fatalf("folder-mode project refreshed default branches: %v", refs)
	}
}
