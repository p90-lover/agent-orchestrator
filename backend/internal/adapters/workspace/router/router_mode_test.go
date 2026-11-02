package router_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// sharedRecordingWorkspace is the git fake plus the git adapter's knowledge of
// a project's shared worktree path.
type sharedRecordingWorkspace struct {
	recordingWorkspace
	root string
}

func (w *sharedRecordingWorkspace) SharedWorktreePath(projectID domain.ProjectID) (string, error) {
	return filepath.Join(w.root, string(projectID), "shared-worktree"), nil
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %v %s", err, out)
	}
	return dir
}

func modeRouter(t *testing.T, mode domain.WorkspaceMode) (*workspacerouter.Workspace, *sharedRecordingWorkspace, string) {
	t.Helper()
	repo := gitRepo(t)
	git := &sharedRecordingWorkspace{root: t.TempDir()}
	r := workspacerouter.New(workspacerouter.Deps{
		Git: git,
		Projects: projectStore{projects: map[string]domain.ProjectRecord{
			"repo": {ID: "repo", Kind: domain.ProjectKindSingleRepo, Path: repo, Config: domain.ProjectConfig{WorkspaceMode: mode}},
		}},
	})
	return r, git, repo
}

func TestFolderModeOpensTheProjectDirectoryWithoutTheGitAdapter(t *testing.T) {
	r, git, repo := modeRouter(t, domain.WorkspaceModeFolder)
	info, err := r.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "repo", SessionID: "repo-1", Kind: domain.KindWorker, Branch: "ao/repo-1/root"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Path != filepath.Clean(repo) || info.Branch != "main" {
		t.Fatalf("Create = %+v; want the project directory on its own branch", info)
	}
	if git.createCalls != 0 {
		t.Fatalf("folder mode created a git worktree (%d calls)", git.createCalls)
	}
}

func TestSharedModeReusesOneWorktreeOnAFixedBranch(t *testing.T) {
	r, git, _ := modeRouter(t, domain.WorkspaceModeShared)
	if _, err := r.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "repo", SessionID: "repo-7", Kind: domain.KindWorker, Branch: "ao/repo-7/root"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !git.lastCreate.Shared || git.lastCreate.Branch != "ao/repo-shared" {
		t.Fatalf("shared Create = %+v; want Shared on branch ao/repo-shared", git.lastCreate)
	}
}

func TestOrchestratorsKeepTheirOwnWorktreeInEveryMode(t *testing.T) {
	r, git, _ := modeRouter(t, domain.WorkspaceModeFolder)
	if _, err := r.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "repo", SessionID: "repo-orchestrator", Kind: domain.KindOrchestrator, Branch: "ao/repo-orchestrator"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if git.createCalls != 1 || git.lastCreate.Shared {
		t.Fatalf("orchestrator Create = %+v (%d calls); want its own worktree", git.lastCreate, git.createCalls)
	}
}

func TestTaskPreparationIsRefusedOutsideWorktreeMode(t *testing.T) {
	for _, mode := range []domain.WorkspaceMode{domain.WorkspaceModeFolder, domain.WorkspaceModeShared} {
		r, _, _ := modeRouter(t, mode)
		if _, err := r.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "repo", SessionID: "repo-2", Kind: domain.KindWorker, Branch: "ao/repo-2/root", FreshBranch: true}); err == nil {
			t.Fatalf("%s: a prepared fresh-branch workspace was created", mode)
		}
	}
}

// The safety property: the project's own directory and its shared worktree are
// never removed, stashed away or replayed onto, whatever mode the project is in
// now (a project switched back to worktrees still has sessions that ran there).
func TestRetainedWorkspacesAreNeverTornDown(t *testing.T) {
	for _, mode := range []domain.WorkspaceMode{domain.WorkspaceModeFolder, domain.WorkspaceModeShared, domain.WorkspaceModeWorktree} {
		r, git, repo := modeRouter(t, mode)
		shared, _ := git.SharedWorktreePath("repo")
		for _, path := range []string{repo, shared} {
			info := ports.WorkspaceInfo{Path: path, ProjectID: "repo", SessionID: "repo-3", Branch: "main"}
			ctx := context.Background()
			if err := r.Destroy(ctx, info); !errors.Is(err, ports.ErrWorkspaceRetained) {
				t.Fatalf("%s %s: Destroy = %v; want ErrWorkspaceRetained", mode, path, err)
			}
			if err := r.ForceDestroy(ctx, info); !errors.Is(err, ports.ErrWorkspaceRetained) {
				t.Fatalf("%s %s: ForceDestroy = %v; want ErrWorkspaceRetained", mode, path, err)
			}
			if _, err := r.DestroyReclaim(ctx, info); !errors.Is(err, ports.ErrWorkspaceRetained) {
				t.Fatalf("%s %s: DestroyReclaim = %v; want ErrWorkspaceRetained", mode, path, err)
			}
			if ref, err := r.StashUncommitted(ctx, info); err != nil || ref != "" {
				t.Fatalf("%s %s: StashUncommitted = %q, %v; want nothing taken", mode, path, ref, err)
			}
			if err := r.ApplyPreserved(ctx, info, "refs/ao/preserved/repo-3"); err != nil {
				t.Fatalf("%s %s: ApplyPreserved = %v", mode, path, err)
			}
			if err := r.DeletePreparedBranch(ctx, info); err != nil {
				t.Fatalf("%s %s: DeletePreparedBranch = %v", mode, path, err)
			}
		}
		if git.destroyCalls+git.forceDestroyCalls+git.stashCalls+git.applyCalls != 0 {
			t.Fatalf("%s: the git adapter was asked to act on a retained workspace", mode)
		}
		// A per-session worktree is still the git adapter's to remove.
		if err := r.Destroy(context.Background(), ports.WorkspaceInfo{Path: filepath.Join(git.root, "repo", "repo-4"), ProjectID: "repo", SessionID: "repo-4"}); err != nil || git.destroyCalls != 1 {
			t.Fatalf("%s: a per-session worktree was not destroyed (err %v, calls %d)", mode, err, git.destroyCalls)
		}
	}
}

func TestAFolderSessionRestoresToTheFolder(t *testing.T) {
	r, git, repo := modeRouter(t, domain.WorkspaceModeWorktree)
	info, err := r.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "repo", SessionID: "repo-5", Path: repo})
	if err != nil || info.Path != filepath.Clean(repo) || git.restoreCalls != 0 {
		t.Fatalf("Restore = %+v, %v (git restores %d); want the folder reopened", info, err, git.restoreCalls)
	}
	if err := r.AddExclude(context.Background(), ports.WorkspaceInfo{Path: repo, ProjectID: "repo"}, "/.ao-attachments/"); err != nil || git.addExcludeCalls != 0 {
		t.Fatalf("AddExclude edited the user's repository exclude file (err %v, calls %d)", err, git.addExcludeCalls)
	}
}

func TestUnknownWorkspaceModeIsRejected(t *testing.T) {
	if err := (domain.ProjectConfig{WorkspaceMode: "inplace"}).Validate(); err == nil {
		t.Fatal("an unknown workspaceMode was accepted")
	}
	for _, mode := range []domain.WorkspaceMode{"", domain.WorkspaceModeWorktree, domain.WorkspaceModeShared, domain.WorkspaceModeFolder} {
		if err := (domain.ProjectConfig{WorkspaceMode: mode}).Validate(); err != nil {
			t.Fatalf("workspaceMode %q rejected: %v", mode, err)
		}
	}
}
