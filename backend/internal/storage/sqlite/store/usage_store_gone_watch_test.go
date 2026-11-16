package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// A source settled because its transcript is gone (complete, artifact_missing)
// belongs to a session that can still be resumed, so the session is not
// terminated. Watched like any other source, it was read again on every refresh,
// failed to open, and went back to error.
func TestSourceSettledAsGoneIsNotWatchedAndKeepsNothingPending(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sess := seedUsageSession(t, s, domain.HarnessClaudeCode)
	now := time.Unix(1700000000, 0).UTC()
	binding := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
		NativeRootID: "claude-root",
		State:        domain.UsageBindingComplete,
	})
	source := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
		BindingID:       binding.ID,
		Kind:            domain.UsageSourceClaudeMain,
		NativeSessionID: binding.NativeRootID,
		ArtifactPath:    "/tmp/claude/root.jsonl",
		FileIdentity:    "root",
		State:           domain.UsageSourceComplete,
	})

	if _, err := s.MarkUsageSourceFailure(ctx, source.ID, 1, domain.UsageErrorArtifactMissing, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	watchable, err := s.ListWatchableUsageSources(ctx)
	mustNoError(t, err)
	pending, err := s.HasPendingUsageDiscovery(ctx)
	mustNoError(t, err)
	if len(watchable) != 1 || watchable[0].ID != source.ID || !pending {
		t.Fatalf("a source still being retried: watchable=%+v pending=%v, want it watched and pending", watchable, pending)
	}

	if _, err := s.MarkUsageSourceState(ctx, source.ID, domain.UsageSourceComplete, domain.UsageErrorArtifactMissing, nil, now); err != nil {
		t.Fatal(err)
	}
	watchable, err = s.ListWatchableUsageSources(ctx)
	mustNoError(t, err)
	pending, err = s.HasPendingUsageDiscovery(ctx)
	mustNoError(t, err)
	if len(watchable) != 0 || pending {
		t.Fatalf("a source settled as gone: watchable=%+v pending=%v, want neither", watchable, pending)
	}
}
