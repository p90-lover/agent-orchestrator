package sessionmanager

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/fslink"
)

func writeProvisionFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sameProvisionFile(t *testing.T, a, b string) bool {
	t.Helper()
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// node_modules is farmed before Symlinks run, so a path listed in both is a
// farm; the venv stays a shared link. An `npm ci` in the workspace then
// replaces only the farm.
func TestProvisionWorkspaceFarmsNodeModulesBeforeSymlinks(t *testing.T) {
	project, workspace := t.TempDir(), t.TempDir()
	for rel, data := range map[string]string{
		"frontend/package.json":                 "{}",
		"frontend/package-lock.json":            "v1",
		"frontend/node_modules/pkg/index.js":    "project pkg\n",
		"frontend/node_modules/.bin/pkg.cmd":    "@node pkg\r\n",
		"requirements.txt":                      "a==1",
		".venv/pyvenv.cfg":                      "home = C:\\Python\n",
		".venv/Lib/site-packages/a/__init__.py": "",
	} {
		writeProvisionFile(t, filepath.Join(project, rel), data)
	}
	for rel, data := range map[string]string{"frontend/package.json": "{}", "frontend/package-lock.json": "v1", "requirements.txt": "a==1"} {
		writeProvisionFile(t, filepath.Join(workspace, rel), data)
	}
	m := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	record := domain.ProjectRecord{ID: "proj", Path: project, Config: domain.ProjectConfig{
		HardLinkFarms: []string{"frontend/node_modules"},
		Symlinks:      []string{"frontend/node_modules", ".venv"},
	}}
	if err := m.provisionWorkspace(context.Background(), record, workspace); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("links unavailable: %v", err)
		}
		t.Fatalf("provisionWorkspace: %v", err)
	}
	farm := filepath.Join(workspace, "frontend", "node_modules")
	if isLink, err := fslink.IsLink(farm); err != nil || isLink {
		t.Fatalf("frontend/node_modules link = %v, %v; want a farm (real directory)", isLink, err)
	}
	projectFile := filepath.Join(project, "frontend", "node_modules", "pkg", "index.js")
	if !sameProvisionFile(t, projectFile, filepath.Join(farm, "pkg", "index.js")) {
		t.Fatal("farm file is not a hard link to the project's file")
	}
	if isLink, err := fslink.IsLink(filepath.Join(workspace, ".venv")); err != nil || !isLink {
		t.Fatalf(".venv link = %v, %v; want the shared link", isLink, err)
	}

	// `npm ci` in the workspace: delete node_modules, install afresh.
	if err := os.RemoveAll(farm); err != nil {
		t.Fatal(err)
	}
	writeProvisionFile(t, filepath.Join(farm, "pkg", "index.js"), "reinstalled\n")
	if data, err := os.ReadFile(projectFile); err != nil || string(data) != "project pkg\n" {
		t.Fatalf("project file after npm ci in the workspace = %q, %v", data, err)
	}
}

func TestApplyHardLinkFarmsSkipsWithoutAbortingTheSpawn(t *testing.T) {
	project, workspace := t.TempDir(), t.TempDir()
	for rel, data := range map[string]string{
		"a/package.json":            "{}",
		"a/package-lock.json":       "v1",
		"a/node_modules/x.js":       "x",
		"b/package.json":            "{}",
		"b/node_modules/y.js":       "y",
		"c/node_modules":            "a file, not a folder",
		"d/node_modules/z/index.js": "z",
	} {
		writeProvisionFile(t, filepath.Join(project, rel), data)
	}
	for rel, data := range map[string]string{
		"a/package.json":      "{}",
		"a/package-lock.json": "v2",
		"b/package.json":      "{}",
		"d/node_modules/mine": "the workspace's own",
	} {
		writeProvisionFile(t, filepath.Join(workspace, rel), data)
	}
	outcomes, err := applyHardLinkFarms(project, workspace, []string{"a/node_modules", "b/node_modules", "c/node_modules", "d/node_modules", "missing/node_modules", " "})
	if err != nil {
		t.Fatalf("applyHardLinkFarms: %v", err)
	}
	got := map[string]farmOutcome{}
	for _, outcome := range outcomes {
		got[outcome.path] = outcome
	}
	if len(outcomes) != 4 {
		t.Fatalf("outcomes = %+v, want a, b, c and d (a missing folder is passed over)", outcomes)
	}
	if o := got["a/node_modules"]; o.built || !strings.Contains(o.reason, "a/package-lock.json differs") {
		t.Fatalf("differing lockfile = %+v, want skipped", o)
	}
	if o := got["b/node_modules"]; !o.built || o.stats.Files != 1 {
		t.Fatalf("matching manifests = %+v, want a farm of 1 file", o)
	}
	if o := got["c/node_modules"]; o.built || o.reason != "not a directory in the project" {
		t.Fatalf("file source = %+v, want skipped", o)
	}
	if o := got["d/node_modules"]; o.built || o.reason != "already present in the workspace" {
		t.Fatalf("existing target = %+v, want skipped", o)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "a", "node_modules")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("skipped farm left a folder: %v", err)
	}
	if !sameProvisionFile(t, filepath.Join(project, "b", "node_modules", "y.js"), filepath.Join(workspace, "b", "node_modules", "y.js")) {
		t.Fatal("b's farm file is not the project's file")
	}

	if _, err := applyHardLinkFarms(project, workspace, []string{"../outside"}); err == nil {
		t.Fatal("a path outside the project was accepted")
	}
}
