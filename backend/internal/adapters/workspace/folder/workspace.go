// Package folder runs a session directly in its project's own directory
// (domain.WorkspaceModeFolder): no worktree, no branch of its own. It only ever
// reads the directory's git state. The workspace router refuses every
// destructive operation on such a path with ports.ErrWorkspaceRetained before
// any adapter could act on it.
package folder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/fspath"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// gitTimeout is generous: on a busy hard disk git took over ten seconds here,
// and a chat that waits is better than one that fails to start.
var gitTimeout = 30 * time.Second

// gitCommand starts git; tests replace it with a git that never answers.
var gitCommand = aoprocess.CommandContext

// errGitSlow is a git that did not answer within gitTimeout. Windows reports
// the killed process as "exit status 1", which used to read as "not a git
// repository" and failed every chat start in a real repository on a busy disk.
var errGitSlow = errors.New("git did not answer in time")

// Open describes the project directory as a session workspace: its path, the
// branch it has checked out ("HEAD" when detached) and the commit it is on, so
// diffs and the session's branch label have something true to show.
func Open(ctx context.Context, cfg ports.WorkspaceConfig, projectPath string) (ports.WorkspaceInfo, error) {
	if strings.TrimSpace(projectPath) == "" {
		return ports.WorkspaceInfo{}, errors.New("folder workspace: project has no path")
	}
	dir := filepath.Clean(projectPath)
	if info, err := os.Stat(dir); err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: %w", err)
	} else if !info.IsDir() {
		return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: %q is not a directory", dir)
	}
	// symbolic-ref names the branch even before its first commit; it fails only
	// on a detached HEAD, which rev-parse then confirms is still a repository.
	// Only git's own "not a git repository" makes the folder not a repository; a
	// git that was slow or stopped leaves it unknown, and the start can be retried.
	branch, err := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if interrupted(err) {
		return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: reading %q: %w", dir, err)
	}
	if err != nil || branch == "" {
		if _, repoErr := git(ctx, dir, "rev-parse", "--git-dir"); repoErr != nil {
			if strings.Contains(repoErr.Error(), "not a git repository") {
				return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: %q is not a git repository: %w", dir, repoErr)
			}
			return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: git could not read %q: %w", dir, repoErr)
		}
		branch = "HEAD"
	}
	// A repository with no commit yet has no HEAD commit; the branch still names it.
	// A slow git is no such repository: an empty base would show the chat wrong diffs.
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if interrupted(err) {
		return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: reading %q: %w", dir, err)
	}
	return ports.WorkspaceInfo{
		Path:      dir,
		Branch:    branch,
		BaseSHA:   head,
		SessionID: cfg.SessionID,
		ProjectID: cfg.ProjectID,
		RepoPath:  dir,
	}, nil
}

// git runs one read-only git command. A failure carries git's own stderr, so
// "not a git repository" can be told from every other error.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := gitCommand(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		// The killed process's own exit error says nothing of why it was killed.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return "", fmt.Errorf("%w (git %s, %s)", errGitSlow, args[0], gitTimeout)
			}
			return "", ctxErr
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if stderr := strings.TrimSpace(string(exit.Stderr)); stderr != "" {
				return "", fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// interrupted is a git that gave no answer: it was too slow, or the start was cancelled.
func interrupted(err error) bool {
	return errors.Is(err, errGitSlow) || errors.Is(err, context.Canceled)
}

// SamePath reports whether two paths name the same directory: cleaned,
// symlinks resolved where they exist, and compared case-insensitively on
// Windows. Used to recognise a project's own directory however it was spelled.
func SamePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	ca, cb := canonical(a), canonical(b)
	if filepath.Separator == '\\' {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

func canonical(p string) string {
	clean := filepath.Clean(p)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	if resolved, err := fspath.Resolve(clean); err == nil {
		clean = resolved
	}
	return strings.TrimRight(clean, `\/`)
}
