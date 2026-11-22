package gitworktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Teardown used to unlink the session's directory inline, inside the request
// that asked for it. A worker checkout carries its build output with it:
// node_modules, target/, .venv, language server caches, none of it tracked by
// git and all of it deleted by `git worktree remove`. Measured on this repo's
// own sessions that is ~2s per 60,000 files, so a real session took tens of
// seconds to kill and a large one ran past the daemon's 60s REST timeout: the
// request context was cancelled mid-teardown, the handler answered 500, and the
// session was left half-dead (agent stopped, row still marked alive).
const (
	pendingDiscardDirName = ".discard-pending"
	// Legacy .discarded may contain worktrees moved before validation.
	discardedDirName = ".discard-approved"
)

func (w *Workspace) discardedRoot() string {
	return filepath.Join(w.managedRoot, discardedDirName)
}

func (w *Workspace) pendingDiscardRoot() string {
	return filepath.Join(w.managedRoot, pendingDiscardDirName)
}

// Pending worktrees may contain uncommitted changes and must survive a restart.
func (w *Workspace) discard(path string) (string, bool) {
	root := w.pendingDiscardRoot()
	for _, dir := range []string{w.discardedRoot(), root} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return "", false
		}
	}
	base := filepath.Base(path)
	for i := range 100 {
		candidate := filepath.Join(root, fmt.Sprintf("%s-%d-%d", base, time.Now().UnixNano(), i))
		if _, err := os.Lstat(candidate); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false
		}
		if err := os.Rename(path, candidate); err != nil {
			return "", false
		}
		return candidate, true
	}
	return "", false
}

// Only a validated or explicitly forced removal may enter the startup sweep.
func (w *Workspace) approveDiscard(pending string) error {
	approved := filepath.Join(w.discardedRoot(), filepath.Base(pending))
	if _, err := os.Lstat(approved); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return fmt.Errorf("gitworktree: checkout preserved at %q: approve discard: %w", pending, err)
	}
	if err := os.Rename(pending, approved); err != nil {
		return fmt.Errorf("gitworktree: checkout preserved at %q: approve discard: %w", pending, err)
	}
	w.removeInBackground(approved)
	return nil
}

// undiscard moves a discarded directory back to its original path. It is the
// rollback for a teardown that turned out to be refusable after the move. The
// directory must survive a refusal, exactly as it did when git itself declined
// to remove it.
func (w *Workspace) undiscard(discarded, path string) error {
	if err := os.Rename(discarded, path); err != nil {
		return fmt.Errorf("gitworktree: cannot restore %q; worktree preserved at %q: %w", path, discarded, err)
	}
	return nil
}

// removeInBackground unlinks a discarded directory off the caller's path. The
// work is tracked so tests (and shutdown) can wait for it; failures are logged
// rather than returned because no caller is left to act on them; a leftover
// under the discard root is swept on the next daemon start.
func (w *Workspace) removeInBackground(path string) {
	w.discards.Add(1)
	go func() {
		defer w.discards.Done()
		if err := removeAllWithRetry(context.Background(), path); err != nil {
			w.logf("gitworktree: could not remove discarded worktree %q: %v", path, err)
		}
	}()
}

// sweepDiscarded removes anything left in the discard root by a previous run.
// A daemon that exits (or is killed) mid-removal would otherwise leak the
// directory forever: nothing else ever looks at this path again.
func (w *Workspace) sweepDiscarded() {
	entries, err := os.ReadDir(w.discardedRoot())
	if err != nil {
		return
	}
	for _, entry := range entries {
		w.removeInBackground(filepath.Join(w.discardedRoot(), entry.Name()))
	}
}

// waitForDiscards blocks until every scheduled background removal has finished.
// Test-only: production callers deliberately do not wait.
func (w *Workspace) waitForDiscards() {
	w.discards.Wait()
}

func (w *Workspace) logf(format string, args ...any) {
	if w.logger == nil {
		return
	}
	w.logger.Warn(fmt.Sprintf(format, args...))
}

// worktreeRegistration reports whether git still has path registered as a
// worktree and whether it is locked. conclusive is false when git could not be
// asked at all, which routes the caller back to the git-driven path instead of
// letting a failed probe look like an answer.
func (w *Workspace) worktreeRegistration(ctx context.Context, repo, path string) (registered, locked, conclusive bool) {
	records, err := w.listRecords(ctx, repo)
	if err != nil {
		return false, false, false
	}
	rec, found := findWorktree(records, path)
	return found, found && rec.Locked, true
}

// worktreeDirty is isDirty with the same "could not ask" distinction as
// worktreeRegistration: an unreadable status is not a clean status. It takes a
// path rather than the worktree's registered location because the fast path
// asks about the directory after it has been moved aside.
func (w *Workspace) worktreeDirty(ctx context.Context, path string) (dirty, conclusive bool) {
	dirty, err := w.isDirty(ctx, path)
	if err != nil {
		return false, false
	}
	return dirty, true
}

// discardWorktree is Destroy's fast path: it applies the same checks git would
// have applied (registered, unlocked, clean), then moves the directory aside
// and prunes the registration instead of waiting for git to walk the tree.
//
// handled=false means "not my business, run the git-driven path": every
// inconclusive probe routes there so git stays the authority on refusals and
// their messages. A refusal discovered after the move restores the directory
// first, because a refused teardown must leave the worktree exactly as it was.
func (w *Workspace) discardWorktree(ctx context.Context, repo, path string) (bool, error) {
	registered, locked, conclusive := w.worktreeRegistration(ctx, repo, path)
	if !conclusive || locked {
		return false, nil
	}
	discarded, moved := w.discard(path)
	if !moved {
		return false, nil
	}
	// Dirtiness is judged AFTER the move, deliberately. Checking first and
	// deleting afterwards leaves a window: teardown stops the agent and gates
	// the session's shells before it gets here, but a stray watcher or
	// background child can still land a file in the worktree between the two,
	// and the delete would then take work the check said was not there. Once
	// the directory has been renamed, the worktree path no longer resolves for
	// anyone, so what git reports here is exactly what gets unlinked. The
	// worktree stays registered until the prune below, which is what keeps
	// `git status` answerable at the new location.
	if registered {
		dirty, conclusive := w.worktreeDirty(ctx, discarded)
		if !conclusive {
			if err := w.undiscard(discarded, path); err != nil {
				return true, err
			}
			return false, nil
		}
		if dirty {
			refusal := fmt.Errorf("gitworktree: refusing to remove %q: %w (worktree has uncommitted changes)", path, ports.ErrWorkspaceDirty)
			return true, errors.Join(refusal, w.undiscard(discarded, path))
		}
	}
	if _, err := w.run(ctx, w.binary, worktreePruneArgs(repo)...); err != nil {
		if restoreErr := w.undiscard(discarded, path); restoreErr != nil {
			return true, errors.Join(fmt.Errorf("gitworktree: worktree prune: %w", err), restoreErr)
		}
		return false, nil //nolint:nilerr // the git-driven path re-runs prune and reports the failure
	}
	// A successful prune can remove the registration even if the follow-up
	// probe fails. Restoring the directory then would reintroduce a stray path.
	stillRegistered, _, conclusive := w.worktreeRegistration(ctx, repo, path)
	if conclusive && stillRegistered {
		refusal := fmt.Errorf("gitworktree: refusing to remove %q: path is still registered after git worktree prune", path)
		return true, errors.Join(refusal, w.undiscard(discarded, path))
	}
	return true, w.approveDiscard(discarded)
}
