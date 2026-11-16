package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const endedSessionClaudeLine = `{"type":"assistant","uuid":"native-message","message":{"id":"msg-1","model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":8,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":2}}}` + "\n"

// seedEndedSessionSource registers one unread Claude transcript for a session
// that has exited but can still be resumed, so it is not terminated and its
// sources stay watched whatever state its binding is in.
func seedEndedSessionSource(
	t *testing.T,
	now time.Time,
) (*sqlite.Store, domain.SessionRecord, domain.UsageBindingRecord, domain.UsageSourceRecord, string) {
	t.Helper()
	store, session := seedUsageTestSession(
		t, t.TempDir(), "usage", domain.HarnessClaudeCode, domain.ActivityExited, "claude-root", now,
	)
	binding := seedUsageTestBinding(t, store, session, "claude-root", domain.UsageBindingActive, now)
	source, path := insertEndedSessionSource(t, store, binding, domain.UsageSourceClaudeMain, "", "claude-root.jsonl", now)
	return store, session, binding, source, path
}

func insertEndedSessionSource(
	t *testing.T,
	store *sqlite.Store,
	binding domain.UsageBindingRecord,
	kind domain.UsageSourceKind,
	subagentID string,
	name string,
	now time.Time,
) (domain.UsageSourceRecord, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	mustNoError(t, os.WriteFile(path, []byte(endedSessionClaudeLine), 0o600))
	path = canonicalTranscriptPath(path)
	identity, err := usagesvc.SourceIdentity(context.Background(), path)
	mustNoError(t, err)
	source, err := store.InsertUsageSource(context.Background(), domain.UsageSourceRecord{
		BindingID:       binding.ID,
		Kind:            kind,
		NativeSessionID: "claude-root",
		SubagentID:      subagentID,
		ArtifactPath:    path,
		FileIdentity:    identity,
		State:           domain.UsageSourcePending,
		UpdatedAt:       now,
	})
	mustNoError(t, err)
	return source, path
}

// missTranscriptThroughReconcilePhase reads a source whose transcript is gone
// for as many misses as still ask for a full reconcile.
func missTranscriptThroughReconcilePhase(ctx context.Context, t *testing.T, ingestor *Ingestor, sourceID int64) {
	t.Helper()
	for miss := int64(1); miss <= missingArtifactReconcileFailures; miss++ {
		result, err := ingestor.Ingest(ctx, sourceID)
		if err == nil || !strings.Contains(err.Error(), domain.UsageErrorArtifactMissing) {
			t.Fatalf("miss %d: error = %v, want %s", miss, err, domain.UsageErrorArtifactMissing)
		}
		if result.RetryAt == nil || !result.Reconcile {
			t.Fatalf("miss %d: result = %+v, want a retry that asks for a reconcile", miss, result)
		}
	}
}

func usageBindingStateOf(ctx context.Context, t *testing.T, store *sqlite.Store, session domain.SessionRecord) domain.UsageBindingRecord {
	t.Helper()
	bindings, err := store.ListUsageBindingsForSession(ctx, session.ID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
	return bindings[0]
}

// A transcript gone for good left its source in error for as long as the daemon
// ran. A finalizing binding then never completed, and any ended but resumable
// session's binding kept discovery pending: the whole discovery pass reran every
// 15 minutes and the source was retried every 6 hours. Past the misses that can
// still find a moved file, the source is now settled instead.
func TestIngestorSettlesEndedSessionSourceWhoseTranscriptIsGone(t *testing.T) {
	tests := []struct {
		name         string
		bindingState domain.UsageBindingState
		want         domain.UsageBindingState
	}{
		{
			name:         "finalizing binding completes as partial",
			bindingState: domain.UsageBindingFinalizing,
			want:         domain.UsageBindingPartial,
		},
		{
			name:         "complete binding stays complete",
			bindingState: domain.UsageBindingComplete,
			want:         domain.UsageBindingComplete,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1700000000, 0).UTC()
			store, session, binding, source, path := seedEndedSessionSource(t, now)
			ingestor := NewIngestor(store, IngestorConfig{Clock: func() time.Time { return now }})
			ingestSourceFully(ctx, t, ingestor, source.ID)
			assertTokenAggregate(t, store, session.ID, 10)
			if test.bindingState == domain.UsageBindingComplete {
				_, err := store.MarkUsageSourceState(ctx, source.ID, domain.UsageSourceComplete, "", nil, now)
				mustNoError(t, err)
			}
			_, err := store.UpdateUsageBindingState(ctx, binding.ID, test.bindingState, "", now)
			mustNoError(t, err)
			mustNoError(t, os.Remove(path))

			missTranscriptThroughReconcilePhase(ctx, t, ingestor, source.ID)
			pending, err := store.HasPendingUsageDiscovery(ctx)
			mustNoError(t, err)
			if !pending {
				t.Fatal("while a moved transcript may still be found, discovery stays pending")
			}

			// Twice: the miss that settles it, then one more from a stale queue entry.
			for _, attempt := range []string{"settling miss", "stale queue entry"} {
				result, err := ingestor.Ingest(ctx, source.ID)
				if err != nil {
					t.Fatalf("%s: error = %v, want the source settled without one", attempt, err)
				}
				if result != (IngestResult{}) {
					t.Fatalf("%s: result = %+v, want nothing scheduled", attempt, result)
				}
				got, ok, err := store.GetUsageSourceForIngestion(ctx, source.ID)
				if err != nil || !ok {
					t.Fatalf("%s: get source: ok=%v err=%v", attempt, ok, err)
				}
				if got.Source.State != domain.UsageSourceComplete ||
					got.Source.LastErrorCode != domain.UsageErrorArtifactMissing ||
					got.Source.FailureCount <= missingArtifactReconcileFailures || got.Source.NextRetryAt != nil {
					t.Fatalf("%s: source = %+v, want complete / artifact_missing with no retry", attempt, got.Source)
				}
				if state := usageBindingStateOf(ctx, t, store, session); state.State != test.want || state.LastErrorCode != "" {
					t.Fatalf("%s: binding = %+v, want %s", attempt, state, test.want)
				}
			}

			pending, err = store.HasPendingUsageDiscovery(ctx)
			mustNoError(t, err)
			if pending {
				t.Fatal("a settled source no longer keeps discovery pending")
			}
			watchable, err := store.ListWatchableUsageSources(ctx)
			mustNoError(t, err)
			if len(watchable) != 0 {
				t.Fatalf("a settled source is no longer watched: %+v", watchable)
			}
			discovery, err := store.ListUsageDiscoveryBindings(ctx, 8)
			mustNoError(t, err)
			if len(discovery) != 0 {
				t.Fatalf("the binding left the discovery queue: %+v", discovery)
			}
			// What was read before the transcript went away is kept.
			assertTokenAggregate(t, store, session.ID, 10)
		})
	}
}

// The settled source must not hold the binding either way: it does not complete
// the binding while another source is still being read, and the binding
// completes (as partial) when that one finishes.
func TestIngestorSettledSourceLetsBindingCompleteWhenLastSourceFinishes(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()
	store, session, binding, mainSource, _ := seedEndedSessionSource(t, now)
	subagent, subagentPath := insertEndedSessionSource(
		t, store, binding, domain.UsageSourceClaudeSubagent, "sub-7", "agent-sub-7.jsonl", now,
	)
	_, err := store.UpdateUsageBindingState(ctx, binding.ID, domain.UsageBindingFinalizing, "", now)
	mustNoError(t, err)
	mustNoError(t, os.Remove(subagentPath))
	ingestor := NewIngestor(store, IngestorConfig{Clock: func() time.Time { return now }})

	missTranscriptThroughReconcilePhase(ctx, t, ingestor, subagent.ID)
	if _, err := ingestor.Ingest(ctx, subagent.ID); err != nil {
		t.Fatalf("settling miss: %v", err)
	}
	if state := usageBindingStateOf(ctx, t, store, session); state.State != domain.UsageBindingFinalizing {
		t.Fatalf("binding = %+v, want it to wait for the main transcript", state)
	}

	result, err := ingestor.Ingest(ctx, mainSource.ID)
	mustNoError(t, err)
	if result.RetryAt == nil {
		t.Fatalf("main transcript result = %+v, want a finalization quiet period", result)
	}
	now = now.Add(defaultFinalizationWait + time.Second)
	if _, err := ingestor.Ingest(ctx, mainSource.ID); err != nil {
		t.Fatal(err)
	}
	if state := usageBindingStateOf(ctx, t, store, session); state.State != domain.UsageBindingPartial {
		t.Fatalf("binding = %+v, want partial: one transcript was never read to its end", state)
	}
	assertTokenAggregate(t, store, session.ID, 10)
}
