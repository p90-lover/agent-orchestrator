package folder

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestHelperSilentGit stands in for a git stuck on a busy disk: it never answers.
func TestHelperSilentGit(t *testing.T) {
	if os.Getenv("FOLDER_TEST_SILENT_GIT") != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestOpenReadsASlowGitAsUnknownNotAsNoRepository(t *testing.T) {
	// Seen live: git took over ten seconds on a busy disk, Windows reported the
	// killed git as "exit status 1", and every chat start in a real repository
	// failed with "is not a git repository".
	oldTimeout, oldCommand := gitTimeout, gitCommand
	t.Cleanup(func() { gitTimeout, gitCommand = oldTimeout, oldCommand })
	gitTimeout = 200 * time.Millisecond
	gitCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperSilentGit$")
		cmd.Env = append(os.Environ(), "FOLDER_TEST_SILENT_GIT=1")
		return cmd
	}

	_, err := Open(context.Background(), ports.WorkspaceConfig{}, t.TempDir())
	if err == nil {
		t.Fatal("a git that never answered opened the workspace")
	}
	if !errors.Is(err, errGitSlow) {
		t.Fatalf("error = %v, want errGitSlow", err)
	}
	if strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("a slow git was reported as no repository: %v", err)
	}
}

func TestOpenTellsARepositoryFromAPlainFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	plain := t.TempDir()
	if _, err := Open(context.Background(), ports.WorkspaceConfig{}, plain); err == nil || !strings.Contains(err.Error(), "is not a git repository") {
		t.Fatalf("plain folder error = %v, want \"is not a git repository\"", err)
	}

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	info, err := Open(context.Background(), ports.WorkspaceConfig{}, repo)
	if err != nil {
		t.Fatalf("Open(repo): %v", err)
	}
	if info.Branch != "main" || info.BaseSHA == "" {
		t.Fatalf("Open(repo) = branch %q base %q, want main and its commit", info.Branch, info.BaseSHA)
	}
}
