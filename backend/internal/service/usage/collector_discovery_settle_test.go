package usage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReconcileSettlesEndedBindingWhoseTranscriptNeverAppeared(t *testing.T) {
	ctx := context.Background()
	store := collectorTestStore(t)
	session := collectorTestSessionWithActivity(
		t, store, domain.HarnessClaudeCode, "never-written", true, domain.ActivityExited,
	)
	binding := seedCollectorUsageBinding(t, store, session, "never-written",
		domain.UsageBindingFinalizing, session.UpdatedAt, domain.UsageErrorSourceDiscoveryPending)
	collector := NewCollector(store, SourceRoots{ClaudeProjects: t.TempDir()}, nil)

	collector.now = func() time.Time { return session.UpdatedAt.Add(undiscoverableTranscriptGrace / 2) }
	mustNoError(t, collector.ReconcileSources(ctx, 8))
	got, ok, err := store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if !ok || got.State != domain.UsageBindingFinalizing {
		t.Fatalf("within the grace period the binding keeps waiting: %+v", got)
	}
	pending, err := store.HasPendingUsageDiscovery(ctx)
	mustNoError(t, err)
	if !pending {
		t.Fatal("a binding still waiting for its transcript keeps discovery pending")
	}

	collector.now = func() time.Time { return session.UpdatedAt.Add(undiscoverableTranscriptGrace + time.Minute) }
	mustNoError(t, collector.ReconcileSources(ctx, 8))
	got, _, err = store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if got.State != domain.UsageBindingPartial || got.LastErrorCode != domain.UsageErrorArtifactMissing {
		t.Fatalf("an ended session's missing transcript settles the binding as partial: %+v", got)
	}
	pending, err = store.HasPendingUsageDiscovery(ctx)
	mustNoError(t, err)
	if pending {
		t.Fatal("a settled binding no longer reruns discovery every retry")
	}
	bindings, err := store.ListUsageDiscoveryBindings(ctx, 8)
	mustNoError(t, err)
	if len(bindings) != 0 {
		t.Fatalf("a settled binding leaves the discovery queue: %+v", bindings)
	}
}

func TestReconcileKeepsWaitingForLiveSessionTranscript(t *testing.T) {
	ctx := context.Background()
	store := collectorTestStore(t)
	session := collectorTestSessionWithActivity(
		t, store, domain.HarnessClaudeCode, "not-yet-written", false, domain.ActivityIdle,
	)
	binding := seedCollectorUsageBinding(t, store, session, "not-yet-written",
		domain.UsageBindingActive, session.UpdatedAt, domain.UsageErrorSourceDiscoveryPending)
	collector := NewCollector(store, SourceRoots{ClaudeProjects: t.TempDir()}, nil)
	collector.now = func() time.Time { return session.UpdatedAt.Add(24 * time.Hour) }

	mustNoError(t, collector.ReconcileSources(ctx, 8))
	got, _, err := store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if got.State == domain.UsageBindingPartial {
		t.Fatalf("a live session's binding is never given up on: %+v", got)
	}
}

func TestContinuationScanReadsForeignTranscriptOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	store := collectorTestStore(t)
	claudeRoot := t.TempDir()
	worktree := t.TempDir()
	session := seedContinuationSession(t, store, claudeRoot, worktree, "native-a")
	seedCollectorUsageBinding(t, store, session, "native-a", domain.UsageBindingActive, now, "")
	foreign := filepath.Join(claudeRoot, "encoded-worktree", continuationNativeB+".jsonl")
	// Marshalled, not concatenated: a Windows temp path's backslashes would
	// otherwise be read as JSON escapes and the record would never parse.
	record, err := json.Marshal(map[string]string{
		"type": "user", "sessionId": continuationNativeB, "cwd": filepath.Join(worktree, "elsewhere"),
	})
	mustNoError(t, err)
	writeUsageFixture(t, foreign, string(record)+"\n")

	collector := NewCollector(store, SourceRoots{ClaudeProjects: claudeRoot}, nil)
	reads := map[string]int{}
	collector.readTranscriptHead = func(ctx context.Context, path string) (string, string, bool) {
		reads[filepath.Base(path)]++
		return readClaudeTranscriptMeta(ctx, path)
	}
	for range 3 {
		mustNoError(t, collector.ReconcileSources(ctx, 8))
	}

	if got := reads[filepath.Base(foreign)]; got != 1 {
		t.Fatalf("a transcript from another workspace was read %d times over three passes, want 1", got)
	}
	if _, ok := continuationBindingIDs(t, store, session.ID)[continuationNativeB]; ok {
		t.Fatal("the cached head still rejects the foreign transcript")
	}
}
