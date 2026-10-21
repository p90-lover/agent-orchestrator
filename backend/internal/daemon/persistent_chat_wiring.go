package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type persistentChatSessionStore interface {
	ListAllSessions(context.Context) ([]domain.SessionRecord, error)
}

// reconcilePersistentChatHosts removes hosts only when durable state proves
// there is no live Chat session to adopt. An unreadable session set is not
// evidence that any host is orphaned.
func reconcilePersistentChatHosts(ctx context.Context, dataDir string, store persistentChatSessionStore) error {
	records, err := store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for persistent chat hosts: %w", err)
	}
	return persistenthost.Reconcile(ctx, dataDir, persistentChatHostKeepSet(records))
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
