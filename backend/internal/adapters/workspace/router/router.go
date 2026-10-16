package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/folder"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ProjectStore is the project lookup surface needed to choose the workspace
// implementation for a session.
type ProjectStore interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// GitWorkspace is the Git-specific capability set required by the router.
type GitWorkspace interface {
	ports.Workspace
	ports.WorkspaceDefaultBranchRefresher
}

// Deps configures a workspace router.
type Deps struct {
	Git      GitWorkspace
	Scratch  ports.Workspace
	Projects ProjectStore
}

// Workspace delegates workspace operations to the adapter that matches the
// session's project kind and workspace mode.
//
// A single-repo project's WorkspaceMode decides where a NEW worker workspace
// goes: its own worktree (default), the project's one shared worktree, or the
// project's own directory. Every operation on an EXISTING workspace is judged
// by its path instead: the project's directory and its shared worktree are
// retained, so Destroy, DestroyReclaim and ForceDestroy refuse them with
// ports.ErrWorkspaceRetained and stash, replay and branch cleanup leave them
// untouched, whatever the project's mode is now.
type Workspace struct {
	git      GitWorkspace
	scratch  ports.Workspace
	projects ProjectStore
}

var _ ports.Workspace = (*Workspace)(nil)
var _ ports.WorkspaceDefaultBranchRefresher = (*Workspace)(nil)
var _ ports.WorkspaceProject = (*Workspace)(nil)
var _ ports.WorkspaceObserver = (*Workspace)(nil)
var _ ports.WorkspaceReclaimer = (*Workspace)(nil)
var _ ports.WorkspacePreparationBranchCleaner = (*Workspace)(nil)

// New returns a router over git and scratch workspace implementations.
func New(deps Deps) *Workspace {
	return &Workspace{
		git:      deps.Git,
		scratch:  deps.Scratch,
		projects: deps.Projects,
	}
}

// sharedPather is the git adapter's knowledge of a project's shared worktree.
type sharedPather interface {
	SharedWorktreePath(projectID domain.ProjectID) (string, error)
}

// sharedBranch is the one branch a project's shared worktree has checked out,
// named like an orchestrator's ("ao/<prefix>-orchestrator").
func sharedBranch(projectID domain.ProjectID) string {
	return "ao/" + string(projectID) + "-shared"
}

// modeFor is where a new workspace for cfg goes. Only a single-repo project's
// worker sessions follow its WorkspaceMode; orchestrators, workspace projects
// and scratch projects keep their own workspaces.
func (w *Workspace) modeFor(ctx context.Context, cfg ports.WorkspaceConfig) (domain.WorkspaceMode, domain.ProjectRecord, error) {
	if cfg.Kind == domain.KindOrchestrator || cfg.ProjectID == "" || w.projects == nil {
		return domain.WorkspaceModeWorktree, domain.ProjectRecord{}, nil
	}
	project, ok, err := w.projects.GetProject(ctx, string(cfg.ProjectID))
	if err != nil {
		return "", domain.ProjectRecord{}, fmt.Errorf("workspace router: project %q: %w", cfg.ProjectID, err)
	}
	if !ok || project.Kind.WithDefault() != domain.ProjectKindSingleRepo {
		return domain.WorkspaceModeWorktree, project, nil
	}
	return project.Config.WorkspaceMode.WithDefault(), project, nil
}

// retained reports whether path is a workspace AO must never remove, reset or
// rewrite: the project's own directory (inFolder), or its shared worktree. A
// project lookup failure is returned, never guessed past.
func (w *Workspace) retained(ctx context.Context, projectID domain.ProjectID, path string) (inFolder bool, retained bool, err error) {
	if path == "" || projectID == "" {
		return false, false, nil
	}
	if w.projects != nil {
		project, ok, err := w.projects.GetProject(ctx, string(projectID))
		if err != nil {
			return false, false, fmt.Errorf("workspace router: project %q: %w", projectID, err)
		}
		if ok && folder.SamePath(path, project.Path) {
			return true, true, nil
		}
	}
	if sp, ok := w.git.(sharedPather); ok {
		if shared, err := sp.SharedWorktreePath(projectID); err == nil && folder.SamePath(path, shared) {
			return false, true, nil
		}
	}
	return false, false, nil
}

// ResolveDefaultBranch delegates local default-branch resolution to the git workspace adapter.
func (w *Workspace) ResolveDefaultBranch(ctx context.Context, repoPath, configuredBranch string) (ports.WorkspaceDefaultBranch, error) {
	return w.git.ResolveDefaultBranch(ctx, repoPath, configuredBranch)
}

// FetchDefaultBranch delegates default-branch fetching to the git workspace adapter.
func (w *Workspace) FetchDefaultBranch(ctx context.Context, repoPath string, target ports.WorkspaceDefaultBranch) error {
	return w.git.FetchDefaultBranch(ctx, repoPath, target)
}

// Create delegates session workspace creation to the project-appropriate
// workspace adapter, or opens the project's folder or shared worktree when its
// WorkspaceMode asks for one.
func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	mode, project, err := w.modeFor(ctx, cfg)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	switch mode {
	case domain.WorkspaceModeFolder:
		if cfg.FreshBranch {
			return ports.WorkspaceInfo{}, errors.New("workspace router: task preparation needs per-session worktrees; this project runs in its folder")
		}
		return folder.Open(ctx, cfg, project.Path)
	case domain.WorkspaceModeShared:
		if cfg.FreshBranch {
			return ports.WorkspaceInfo{}, errors.New("workspace router: task preparation needs per-session worktrees; this project shares one worktree")
		}
		cfg.Shared = true
		cfg.Branch = sharedBranch(cfg.ProjectID)
		return w.git.Create(ctx, cfg)
	}
	adapter, err := w.adapterForProject(ctx, cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return adapter.Create(ctx, cfg)
}

// Restore delegates session workspace restoration to the project-appropriate
// workspace adapter. A session that ran in its project's folder reopens that
// folder; one that ran in the shared worktree reattaches to it.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	inFolder, retained, err := w.retained(ctx, cfg.ProjectID, cfg.Path)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if inFolder {
		return folder.Open(ctx, cfg, cfg.Path)
	}
	if retained {
		cfg.Shared = true
		return w.git.Restore(ctx, cfg)
	}
	adapter, err := w.adapterForProject(ctx, cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return adapter.Restore(ctx, cfg)
}

// Destroy delegates normal session workspace cleanup to the
// project-appropriate workspace adapter. A retained workspace is refused.
func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return err
	} else if retained {
		return ports.ErrWorkspaceRetained
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.Destroy(ctx, info)
}

// DestroyReclaim delegates teardown to the project-appropriate adapter and
// reports whether that adapter actually released anything. An adapter that does
// not report reclaim outcomes falls back to Destroy, whose nil error keeps the
// pre-existing meaning of a completed removal. A retained workspace is refused.
func (w *Workspace) DestroyReclaim(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceReclaim, error) {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return ports.WorkspaceReclaimAlreadyAbsent, err
	} else if retained {
		return ports.WorkspaceReclaimAlreadyAbsent, ports.ErrWorkspaceRetained
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return ports.WorkspaceReclaimAlreadyAbsent, err
	}
	if reclaimer, ok := adapter.(ports.WorkspaceReclaimer); ok {
		return reclaimer.DestroyReclaim(ctx, info)
	}
	return ports.WorkspaceReclaimRemoved, adapter.Destroy(ctx, info)
}

// DeletePreparedBranch delegates safe speculative-branch cleanup to Git
// workspaces. A retained workspace's branch is never deleted.
func (w *Workspace) DeletePreparedBranch(ctx context.Context, info ports.WorkspaceInfo) error {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return err
	} else if retained {
		return nil
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	if cleaner, ok := adapter.(ports.WorkspacePreparationBranchCleaner); ok {
		return cleaner.DeletePreparedBranch(ctx, info)
	}
	return nil
}

// ForceDestroy delegates forced session workspace cleanup to the
// project-appropriate workspace adapter. A retained workspace is refused: no
// caller's need to clean up outweighs keeping a user's folder.
func (w *Workspace) ForceDestroy(ctx context.Context, info ports.WorkspaceInfo) error {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return err
	} else if retained {
		return ports.ErrWorkspaceRetained
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.ForceDestroy(ctx, info)
}

// StashUncommitted delegates preservation of dirty workspace state to the
// project-appropriate workspace adapter. A retained workspace keeps its work
// where it is, so there is nothing to preserve elsewhere.
func (w *Workspace) StashUncommitted(ctx context.Context, info ports.WorkspaceInfo) (string, error) {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return "", err
	} else if retained {
		return "", nil
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return "", err
	}
	return adapter.StashUncommitted(ctx, info)
}

// ApplyPreserved delegates restored dirty workspace state application to the
// project-appropriate workspace adapter. Nothing is ever replayed onto a
// retained workspace: its work never left it.
func (w *Workspace) ApplyPreserved(ctx context.Context, info ports.WorkspaceInfo, ref string) error {
	if _, retained, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return err
	} else if retained {
		return nil
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.ApplyPreserved(ctx, info, ref)
}

// AddExclude delegates local workspace ignore updates to the project-appropriate
// workspace adapter. A project's own folder is left as the user has it: its
// repository's exclude file is not AO's to edit.
func (w *Workspace) AddExclude(ctx context.Context, info ports.WorkspaceInfo, patterns ...string) error {
	if inFolder, _, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return err
	} else if inFolder {
		return nil
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return err
	}
	return adapter.AddExclude(ctx, info, patterns...)
}

// ObserveWorkspace delegates the read-only handoff snapshot to the selected
// project adapter. Adapters that cannot observe state return a clear error
// instead of fabricating repository facts.
func (w *Workspace) ObserveWorkspace(ctx context.Context, info ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	if inFolder, _, err := w.retained(ctx, info.ProjectID, info.Path); err != nil {
		return ports.WorkspaceObservation{}, err
	} else if inFolder {
		return ports.WorkspaceObservation{}, errors.New("workspace router: a session in its project's folder has no workspace snapshot")
	}
	adapter, err := w.adapterForProject(ctx, info.ProjectID)
	if err != nil {
		return ports.WorkspaceObservation{}, err
	}
	observer, ok := adapter.(ports.WorkspaceObserver)
	if !ok {
		return ports.WorkspaceObservation{}, errors.New("workspace router: selected workspace does not support observation")
	}
	return observer.ObserveWorkspace(ctx, info)
}

// CreateWorkspaceProject delegates root-as-repo workspace project creation to
// the git adapter.
func (w *Workspace) CreateWorkspaceProject(ctx context.Context, cfg ports.WorkspaceProjectConfig) (ports.WorkspaceProjectInfo, error) {
	gitProject, err := w.gitWorkspaceProject()
	if err != nil {
		return ports.WorkspaceProjectInfo{}, err
	}
	return gitProject.CreateWorkspaceProject(ctx, cfg)
}

// DestroyWorkspaceProject delegates root-as-repo workspace project cleanup to
// the git adapter.
func (w *Workspace) DestroyWorkspaceProject(ctx context.Context, info ports.WorkspaceProjectInfo) error {
	gitProject, err := w.gitWorkspaceProject()
	if err != nil {
		return err
	}
	return gitProject.DestroyWorkspaceProject(ctx, info)
}

func (w *Workspace) adapterForProject(ctx context.Context, projectID domain.ProjectID) (ports.Workspace, error) {
	if w == nil {
		return nil, errors.New("workspace router: nil router")
	}
	// Projectless sessions use AO-managed plain directories. They deliberately
	// have no Git worktree, branch, tracker, or project row behind them.
	if projectID == "" {
		if w.scratch == nil {
			return nil, errors.New("workspace router: standalone workspace is not configured")
		}
		return w.scratch, nil
	}
	if w.projects != nil && projectID != "" {
		project, ok, err := w.projects.GetProject(ctx, string(projectID))
		if err != nil {
			return nil, fmt.Errorf("workspace router: project %q: %w", projectID, err)
		}
		if ok && project.Kind.WithDefault() == domain.ProjectKindScratch {
			if w.scratch == nil {
				return nil, errors.New("workspace router: scratch workspace is not configured")
			}
			return w.scratch, nil
		}
	}
	if w.git == nil {
		return nil, errors.New("workspace router: git workspace is not configured")
	}
	return w.git, nil
}

func (w *Workspace) gitWorkspaceProject() (ports.WorkspaceProject, error) {
	if w == nil || w.git == nil {
		return nil, errors.New("workspace router: git workspace is not configured")
	}
	gitProject, ok := w.git.(ports.WorkspaceProject)
	if !ok {
		return nil, errors.New("workspace router: git workspace does not support workspace projects")
	}
	return gitProject, nil
}
