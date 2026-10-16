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
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

const gitTimeout = 10 * time.Second

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
	branch, err := git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || branch == "" {
		if _, repoErr := git(ctx, dir, "rev-parse", "--git-dir"); repoErr != nil {
			return ports.WorkspaceInfo{}, fmt.Errorf("folder workspace: %q is not a git repository: %w", dir, repoErr)
		}
		branch = "HEAD"
	}
	// A repository with no commit yet has no HEAD commit; the branch still names it.
	head, _ := git(ctx, dir, "rev-parse", "HEAD")
	return ports.WorkspaceInfo{
		Path:      dir,
		Branch:    branch,
		BaseSHA:   head,
		SessionID: cfg.SessionID,
		ProjectID: cfg.ProjectID,
		RepoPath:  dir,
	}, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := aoprocess.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return strings.TrimRight(clean, `\/`)
}
