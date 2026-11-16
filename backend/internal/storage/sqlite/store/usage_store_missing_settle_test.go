package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSettleMissingUsageSourceCompletesItsFinalizingBinding(t *testing.T) {
	tests := []struct {
		name          string
		otherState    domain.UsageSourceState
		wantCompleted bool
		wantBinding   domain.UsageBindingState
	}{
		{
			name:          "last source the binding waited for",
			otherState:    domain.UsageSourceComplete,
			wantCompleted: true,
			wantBinding:   domain.UsageBindingPartial,
		},
		{
			name:        "another source is still being read",
			otherState:  domain.UsageSourceActive,
			wantBinding: domain.UsageBindingFinalizing,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			sess := seedUsageSession(t, s, domain.HarnessClaudeCode)
			now := time.Unix(1700000000, 0).UTC()
			binding := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
				NativeRootID: "claude-root",
				State:        domain.UsageBindingFinalizing,
			})
			mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
				BindingID:       binding.ID,
				Kind:            domain.UsageSourceClaudeMain,
				NativeSessionID: binding.NativeRootID,
				ArtifactPath:    "/tmp/claude/root.jsonl",
				FileIdentity:    "root",
				State:           tt.otherState,
			})
			gone := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
				BindingID:       binding.ID,
				Kind:            domain.UsageSourceClaudeSubagent,
				NativeSessionID: binding.NativeRootID,
				SubagentID:      "sub-7",
				ArtifactPath:    "/tmp/claude/root/subagents/agent-sub-7.jsonl",
				FileIdentity:    "sub-7",
				State:           domain.UsageSourcePending,
			})
			if _, err := s.MarkUsageSourceFailure(ctx, gone.ID, 3, domain.UsageErrorArtifactMissing, now.Add(time.Hour), now); err != nil {
				t.Fatal(err)
			}

			completed, err := s.SettleMissingUsageSource(ctx, gone.ID, binding.ID, 4, now)
			mustNoError(t, err)
			if completed != tt.wantCompleted {
				t.Fatalf("completed = %v, want %v", completed, tt.wantCompleted)
			}
			got, ok, err := s.GetUsageSourceForIngestion(ctx, gone.ID)
			if err != nil || !ok {
				t.Fatalf("get source: ok=%v err=%v", ok, err)
			}
			if got.Source.State != domain.UsageSourceComplete ||
				got.Source.LastErrorCode != domain.UsageErrorArtifactMissing ||
				got.Source.FailureCount != 4 || got.Source.NextRetryAt != nil {
				t.Fatalf("settled source = %+v", got.Source)
			}
			gotBinding, ok, err := s.GetUsageBinding(ctx, sess.ID, sess.Harness, binding.NativeRootID)
			if err != nil || !ok || gotBinding.State != tt.wantBinding {
				t.Fatalf("binding = %+v, ok=%v err=%v, want %s", gotBinding, ok, err, tt.wantBinding)
			}
		})
	}
}

func TestSettleMissingUsageSourceRejectsUnknownSource(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.SettleMissingUsageSource(context.Background(), 4242, 1, 4, time.Unix(1700000000, 0).UTC()); err == nil {
		t.Fatal("settling a source that does not exist reported success")
	}
}
