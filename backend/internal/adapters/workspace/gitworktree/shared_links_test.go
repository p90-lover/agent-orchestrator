package gitworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/fslink"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestTeardownNeverDeletesThroughSharedLinks links a project dependency folder
// into a worktree the way provisioning does, then tears the worktree down by
// each removal path and asserts the project's own file survives.
func TestTeardownNeverDeletesThroughSharedLinks(t *testing.T) {
	cases := []struct {
		name string
		// gitPath blocks the discard fast path so `git worktree remove` runs.
		gitPath bool
		force   bool
	}{
		{name: "destroy discard"},
		{name: "destroy git", gitPath: true},
		{name: "force discard", force: true},
		{name: "force git", gitPath: true, force: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			git := requireGit(t)
			tmp := t.TempDir()
			repo := setupOriginClone(t, git, tmp)
			ignoreNodeModules(t, repo)
			deps := filepath.Join(tmp, "project-deps", "node_modules")
			if err := os.MkdirAll(filepath.Join(deps, "pkg"), 0o750); err != nil {
				t.Fatal(err)
			}
			kept := filepath.Join(deps, "pkg", "index.js")
			if err := os.WriteFile(kept, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(tmp, "managed")
			ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			if tc.gitPath {
				if err := os.MkdirAll(root, 0o750); err != nil {
					t.Fatal(err)
				}
				// A file where the discard directory belongs makes discard fail.
				if err := os.WriteFile(filepath.Join(root, discardedDirName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-links", Branch: "feature/links-" + string(rune('a'+i))})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			link := filepath.Join(info.Path, "node_modules")
			if _, err := fslink.Link(deps, link); err != nil {
				if runtime.GOOS != "windows" {
					t.Skipf("symlinks unavailable: %v", err)
				}
				t.Fatalf("link: %v", err)
			}
			if tc.force {
				err = ws.ForceDestroy(ctx, info)
			} else {
				err = ws.Destroy(ctx, info)
			}
			if err != nil {
				t.Fatalf("teardown: %v", err)
			}
			ws.waitForDiscards()
			if _, err := os.Lstat(info.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree still present: %v", err)
			}
			if data, err := os.ReadFile(kept); err != nil || string(data) != "keep" {
				t.Fatalf("project dependency file = %q, %v; teardown deleted through the link", data, err)
			}
		})
	}
}

// A refused Destroy keeps the worktree, and the worktree keeps its links.
func TestRefusedDestroyRestoresSharedLinks(t *testing.T) {
	git := requireGit(t)
	tmp := t.TempDir()
	repo := setupOriginClone(t, git, tmp)
	ignoreNodeModules(t, repo)
	deps := filepath.Join(tmp, "project-deps", "node_modules")
	if err := os.MkdirAll(deps, 0o750); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "managed")
	ws, err := New(Options{Binary: git, ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, discardedDirName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess-refused", Branch: "feature/links-refused"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	link := filepath.Join(info.Path, "node_modules")
	if _, err := fslink.Link(deps, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("link: %v", err)
	}
	if err := os.WriteFile(filepath.Join(info.Path, "wip.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ws.Destroy(ctx, info); !errors.Is(err, ports.ErrWorkspaceDirty) {
		t.Fatalf("Destroy dirty error = %v, want ErrWorkspaceDirty", err)
	}
	if isLink, err := fslink.IsLink(link); err != nil || !isLink {
		t.Fatalf("dependency link after refused Destroy = %v, %v; want restored", isLink, err)
	}
}

// ignoreNodeModules ignores node_modules for every worktree of repo, as a
// project's .gitignore does, so the link is not untracked work.
func ignoreNodeModules(t *testing.T, repo string) {
	t.Helper()
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte("node_modules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
