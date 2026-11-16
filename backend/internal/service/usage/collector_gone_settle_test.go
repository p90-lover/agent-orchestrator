package usage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// An exited session can be resumed, so it is never terminated. Its binding kept
// waiting for a transcript that was never written, which kept discovery pending
// for as long as the daemon ran.
func TestReconcileSettlesExitedBindingWhoseTranscriptNeverAppeared(t *testing.T) {
	ctx := context.Background()
	store := collectorTestStore(t)
	session := collectorTestSessionWithActivity(
		t, store, domain.HarnessClaudeCode, "never-written-exited", false, domain.ActivityExited,
	)
	binding := seedCollectorUsageBinding(t, store, session, "never-written-exited",
		domain.UsageBindingFinalizing, session.UpdatedAt, domain.UsageErrorSourceDiscoveryPending)
	collector := NewCollector(store, SourceRoots{ClaudeProjects: t.TempDir()}, nil)

	collector.now = func() time.Time { return session.UpdatedAt.Add(undiscoverableTranscriptGrace / 2) }
	mustNoError(t, collector.ReconcileSources(ctx, 8))
	got, ok, err := store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if !ok || got.State != domain.UsageBindingFinalizing {
		t.Fatalf("within the grace period the binding keeps waiting: %+v", got)
	}

	collector.now = func() time.Time { return session.UpdatedAt.Add(undiscoverableTranscriptGrace + time.Minute) }
	mustNoError(t, collector.ReconcileSources(ctx, 8))
	got, _, err = store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if got.State != domain.UsageBindingPartial || got.LastErrorCode != domain.UsageErrorArtifactMissing {
		t.Fatalf("an exited session's missing transcript settles the binding as partial: %+v", got)
	}
	pending, err := store.HasPendingUsageDiscovery(ctx)
	mustNoError(t, err)
	if pending {
		t.Fatal("a settled binding no longer keeps discovery pending")
	}
	bindings, err := store.ListUsageDiscoveryBindings(ctx, 8)
	mustNoError(t, err)
	if len(bindings) != 0 {
		t.Fatalf("a settled binding leaves the discovery queue: %+v", bindings)
	}
}

// A source settled because its transcript was gone is not watched, so nothing
// reads it again on its own. When discovery finds the transcript where it was,
// the source comes back and the binding waits for it instead of completing as
// partial.
func TestReconcileReactivatesSettledSourceWhoseTranscriptIsFoundAgain(t *testing.T) {
	ctx := context.Background()
	store := collectorTestStore(t)
	const nativeID = "claude-back"
	session := collectorTestSessionWithActivity(
		t, store, domain.HarnessClaudeCode, nativeID, false, domain.ActivityExited,
	)
	root := filepath.Join(t.TempDir(), "projects")
	path := filepath.Join(root, "workspace", nativeID+".jsonl")
	writeUsageFixture(t, path, `{"type":"assistant"}`+"\n")
	path, err := filepath.EvalSymlinks(path)
	mustNoError(t, err)
	now := time.Now().UTC()
	binding := seedCollectorUsageBinding(t, store, session, nativeID, domain.UsageBindingFinalizing, now, "")
	identity, err := SourceIdentity(ctx, path)
	mustNoError(t, err)
	source, err := store.InsertUsageSource(ctx, domain.UsageSourceRecord{
		BindingID:       binding.ID,
		Kind:            domain.UsageSourceClaudeMain,
		NativeSessionID: nativeID,
		ArtifactPath:    path,
		FileIdentity:    identity,
		State:           domain.UsageSourceComplete,
		FailureCount:    4,
		LastErrorCode:   domain.UsageErrorArtifactMissing,
		UpdatedAt:       now,
	})
	mustNoError(t, err)

	collector := NewCollector(store, SourceRoots{ClaudeProjects: root}, nil)
	mustNoError(t, collector.ReconcileSources(ctx, 8))

	sources, err := store.ListUsageSourcesForBinding(ctx, binding.ID)
	mustNoError(t, err)
	if len(sources) != 1 || sources[0].ID != source.ID {
		t.Fatalf("sources = %+v, want the one settled source and no new generation", sources)
	}
	if sources[0].State != domain.UsageSourceActive || sources[0].FailureCount != 0 || sources[0].LastErrorCode != "" {
		t.Fatalf("source = %+v, want it reactivated", sources[0])
	}
	got, _, err := store.GetUsageBinding(ctx, session.ID, binding.Harness, binding.NativeRootID)
	mustNoError(t, err)
	if got.State != domain.UsageBindingFinalizing {
		t.Fatalf("binding = %+v, want it to wait for the transcript that is back", got)
	}
}
