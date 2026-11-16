package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// One reviewer whose restore never returns must not hold up the ones after it.
// With only the daemon's context it did, for as long as the daemon ran.
func TestRecoverChatReviewersGivesEachReviewerItsOwnDeadline(t *testing.T) {
	previous := chatReviewerRecoveryTimeout
	chatReviewerRecoveryTimeout = 50 * time.Millisecond
	t.Cleanup(func() { chatReviewerRecoveryTimeout = previous })

	stuck := chatReviewRow("rev-stuck", domain.ReviewerCodex, "")
	fine := chatReviewRow("rev-fine", domain.ReviewerOpenCode, "")
	store := &fakeStore{
		recoverableReviews: []domain.Review{stuck, fine},
		reviews: map[domain.ReviewerHarness]domain.Review{
			domain.ReviewerCodex: stuck, domain.ReviewerOpenCode: fine,
		},
	}
	launcher := &handleAwareLauncher{
		fakeLauncher: &fakeLauncher{interfaceMode: domain.ReviewerInterfaceChat},
		hang:         map[string]bool{"rev-stuck": true},
	}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, fakePRs{}, fakeProjects{}, launcher)

	result := make(chan error, 1)
	go func() { result <- eng.RecoverChatReviewers(context.Background()) }()
	var err error
	select {
	case err = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery is still waiting on the reviewer whose restore never returns")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RecoverChatReviewers = %v, want the stuck reviewer's deadline reported", err)
	}
	if len(launcher.restored) != 1 || launcher.restored[0] != "rev-fine" {
		t.Fatalf("restored = %v, want the reviewer after the stuck one restored", launcher.restored)
	}
	if store.recoveryErrors["rev-stuck"] == "" {
		t.Fatalf("recorded errors = %v, want the stuck reviewer's failure recorded", store.recoveryErrors)
	}
}
