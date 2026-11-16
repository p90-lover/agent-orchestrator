package review

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// handleAwareLauncher answers Alive per handle and can hold a restore until its
// context ends, which the shared fake launcher cannot.
type handleAwareLauncher struct {
	*fakeLauncher
	live     map[string]bool
	hang     map[string]bool
	restored []string
}

func (l *handleAwareLauncher) Alive(_ context.Context, handleID, _ string) (bool, error) {
	return l.live[handleID], nil
}

func (l *handleAwareLauncher) RestoreTerminal(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	if l.hang[spec.ReviewSessionID] {
		<-ctx.Done()
		return LaunchResult{}, ctx.Err()
	}
	l.restored = append(l.restored, spec.ReviewSessionID)
	return LaunchResult{HandleID: "handle-" + spec.ReviewSessionID, LaunchID: spec.LaunchID}, nil
}

func chatReviewRow(id string, harness domain.ReviewerHarness, handle string) domain.Review {
	return domain.Review{
		ID: id, SessionID: "mer-1", ProjectID: "mer", Harness: harness,
		InterfaceMode: domain.ReviewerInterfaceChat, ProviderConversationID: "conv-" + id,
		ReviewerHandleID: handle, ReviewerLaunchID: "launch-" + handle,
	}
}

// Boot recovery lists the reviewer chats first and takes each worker's lock
// later, while the daemon already serves requests. A trigger or a restore for
// the same worker can relaunch the reviewer in between. Recovery then acted on
// the row it had listed, found that row's old handle dead and launched the same
// reviewer a second time.
func TestRecoverChatReviewersActsOnTheReviewAsItIsUnderTheWorkerLock(t *testing.T) {
	listed := chatReviewRow("rev-1", domain.ReviewerCodex, "handle-old")
	current := chatReviewRow("rev-1", domain.ReviewerCodex, "handle-new")
	store := &fakeStore{
		recoverableReviews: []domain.Review{listed},
		reviews:            map[domain.ReviewerHarness]domain.Review{domain.ReviewerCodex: current},
	}
	launcher := &handleAwareLauncher{
		fakeLauncher: &fakeLauncher{interfaceMode: domain.ReviewerInterfaceChat},
		live:         map[string]bool{"handle-new": true},
	}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, fakePRs{}, fakeProjects{}, launcher)

	if err := eng.RecoverChatReviewers(context.Background()); err != nil {
		t.Fatalf("RecoverChatReviewers: %v", err)
	}
	if len(launcher.restored) != 0 {
		t.Fatalf("recovery launched %v again although the reviewer was already running", launcher.restored)
	}
}

// A review that stopped being a reviewer chat after it was listed is left alone.
func TestRecoverChatReviewersSkipsAReviewThatIsNoLongerAChat(t *testing.T) {
	listed := chatReviewRow("rev-1", domain.ReviewerCodex, "")
	current := listed
	current.InterfaceMode = domain.ReviewerInterfaceTUI
	current.ProviderConversationID = ""
	store := &fakeStore{
		recoverableReviews: []domain.Review{listed},
		reviews:            map[domain.ReviewerHarness]domain.Review{domain.ReviewerCodex: current},
	}
	launcher := &handleAwareLauncher{fakeLauncher: &fakeLauncher{interfaceMode: domain.ReviewerInterfaceChat}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, fakePRs{}, fakeProjects{}, launcher)

	if err := eng.RecoverChatReviewers(context.Background()); err != nil {
		t.Fatalf("RecoverChatReviewers: %v", err)
	}
	if len(launcher.restored) != 0 {
		t.Fatalf("recovery restored %v, which is no longer a reviewer chat", launcher.restored)
	}
}
