package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type persistentChatSessionStore interface {
	ListAllSessions(context.Context) ([]domain.SessionRecord, error)
}

type persistentChatHostStore interface {
	persistentChatSessionStore
	ListRecoverableChatReviews(context.Context) ([]domain.Review, error)
}

// reconcilePersistentChatHosts removes hosts only when durable state proves
// there is no live Chat session or reviewer chat to adopt. An unreadable
// session or review set is not evidence that any host is orphaned.
func reconcilePersistentChatHosts(ctx context.Context, dataDir string, store persistentChatHostStore) error {
	records, err := store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for persistent chat hosts: %w", err)
	}
	reviews, err := store.ListRecoverableChatReviews(ctx)
	if err != nil {
		return fmt.Errorf("list reviewer chats for persistent chat hosts: %w", err)
	}
	keep := persistentChatHostKeepSet(records)
	keepReviewerChatHosts(keep, records, reviews)
	return persistenthost.Reconcile(ctx, dataDir, keep)
}

// keepReviewerChatHosts adds the hosts of the reviewer chats that boot recovery
// restores (review.Engine.RecoverChatReviewers).
//
// A reviewer has no session of its own: its host is named after its review
// (chatsvc.ReviewerHostID). The session ids in the keep set never matched it, so
// the reconcile shut down every reviewer host, including the ones recovery had
// just restored. A reviewer whose worker is gone, terminated or without a
// workspace is not recovered, and its host is not kept.
func keepReviewerChatHosts(keep map[string]struct{}, records []domain.SessionRecord, reviews []domain.Review) {
	workers := make(map[domain.SessionID]struct{}, len(records))
	for _, rec := range records {
		if !rec.IsTerminated && rec.Metadata.WorkspacePath != "" {
			workers[rec.ID] = struct{}{}
		}
	}
	for _, review := range reviews {
		if _, ok := workers[review.SessionID]; ok {
			keep[string(chatsvc.ReviewerHostID(review.ID))] = struct{}{}
		}
	}
}

func persistentChatHostKeepSet(records []domain.SessionRecord) map[string]struct{} {
	keep := make(map[string]struct{})
	for _, rec := range records {
		if rec.IsTerminated || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat {
			continue
		}
		// Boot does not relaunch an exited Chat, so nothing adopts its host; a
		// surviving one only holds its provider, worktree and prompt journal.
		// Resume Agent starts a new host when the user asks for the chat again.
		if rec.Activity.State == domain.ActivityExited {
			continue
		}
		keep[string(rec.ID)] = struct{}{}
	}
	return keep
}

const (
	// exitedChatHostSweepEvery is how often the running daemon looks for hosts
	// whose Chat exited after boot (see persistenthost.ReapExited).
	exitedChatHostSweepEvery = 5 * time.Minute
	// exitedChatHostGrace leaves a just-exited Chat's host alone for a while, so
	// a sweep never races the exit's own cleanup or an immediate resume.
	exitedChatHostGrace = 2 * time.Minute
)

// sweepExitedChatHosts stops the hosts of Chats that exited at least
// exitedChatHostGrace before now. Boot's reconcile handles Chats that were
// already exited; this catches those that exit later, which nothing else stops.
func sweepExitedChatHosts(ctx context.Context, dataDir string, store persistentChatSessionStore, now time.Time) error {
	records, err := store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for exited chat hosts: %w", err)
	}
	return persistenthost.ReapExited(ctx, dataDir, exitedChatHosts(records, now))
}

func exitedChatHosts(records []domain.SessionRecord, now time.Time) map[string]time.Time {
	exited := make(map[string]time.Time)
	for _, rec := range records {
		if domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.Activity.State != domain.ActivityExited {
			continue
		}
		at := rec.Activity.LastActivityAt
		if at.IsZero() || now.Sub(at) < exitedChatHostGrace {
			continue
		}
		exited[string(rec.ID)] = at
	}
	return exited
}

func runExitedChatHostSweeps(ctx context.Context, dataDir string, store persistentChatSessionStore, log *slog.Logger) {
	ticker := time.NewTicker(exitedChatHostSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sweepExitedChatHosts(ctx, dataDir, store, time.Now()); err != nil {
				log.Warn("exited chat host sweep failed", "err", err)
			}
		}
	}
}
