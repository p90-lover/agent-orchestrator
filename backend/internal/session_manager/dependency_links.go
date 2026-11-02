package sessionmanager

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/fslink"
)

// A linked dependency folder is shared, not copied: an install run inside the
// session (npm install, npm ci, pip install) rewrites the project's own
// node_modules or .venv, which the main checkout and every other session see.
// `npm ci` deletes the whole folder first. Sharing is only sound while the
// workspace wants exactly what the project has installed, so a dependency
// folder is linked only when its manifests and lockfiles, the files next to it
// that decide its contents, are byte-identical in the project checkout and in
// the new workspace. A branch with a changed lockfile, or a project with an
// uncommitted manifest edit, gets no link; the agent installs its own copy into
// the workspace instead.
//
// A HardLinkFarms entry (the safer way to give a workspace node_modules) is not
// shared: an install in the workspace replaces only the farm's links. The same
// guard still applies to it, so a workspace that wants different packages
// starts with none rather than with the project's.
//
// Only folders named here are guarded. Any other Symlinks entry (.env, a
// config file) links as before.
var dependencyManifests = map[string][]string{
	"node_modules": {"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"},
	".venv":        pythonManifests,
	"venv":         pythonManifests,
}

var pythonManifests = []string{"pyproject.toml", "uv.lock", "poetry.lock", "Pipfile", "Pipfile.lock", "setup.py", "setup.cfg", "requirements*.txt"}

// dependencyLinkMismatch returns why the dependency folder at the repo-relative
// path rel must not be linked from projectPath into workspacePath, or "" when
// it may (it is not a dependency folder, or every manifest beside it matches).
// A manifest present on one side only is a mismatch; one missing on both sides
// is not.
func dependencyLinkMismatch(projectPath, workspacePath, rel string) string {
	patterns, ok := dependencyManifests[filepath.Base(rel)]
	if !ok {
		return ""
	}
	dir := filepath.Dir(rel)
	projectDir, workspaceDir := filepath.Join(projectPath, dir), filepath.Join(workspacePath, dir)
	names := map[string]bool{}
	for _, pattern := range patterns {
		for _, base := range []string{projectDir, workspaceDir} {
			matches, _ := filepath.Glob(filepath.Join(base, pattern))
			for _, match := range matches {
				names[filepath.Base(match)] = true
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		if reason := manifestDiffers(filepath.Join(projectDir, name), filepath.Join(workspaceDir, name)); reason != "" {
			return filepath.ToSlash(filepath.Join(dir, name)) + " " + reason + "; not linking the project's " + filepath.Base(rel)
		}
	}
	return ""
}

// manifestDiffers compares one manifest in the project and the workspace.
func manifestDiffers(projectFile, workspaceFile string) string {
	project, projectErr := os.ReadFile(projectFile)       //nolint:gosec // repo-relative manifest under the project root
	workspace, workspaceErr := os.ReadFile(workspaceFile) //nolint:gosec // repo-relative manifest under the AO workspace
	projectMissing, workspaceMissing := errors.Is(projectErr, fs.ErrNotExist), errors.Is(workspaceErr, fs.ErrNotExist)
	switch {
	case projectMissing && workspaceMissing:
		return ""
	case projectMissing:
		return "exists only in the workspace"
	case workspaceMissing:
		return "exists only in the project (uncommitted or not on this branch)"
	case projectErr != nil || workspaceErr != nil:
		return "could not be compared: " + strings.TrimSpace(errors.Join(projectErr, workspaceErr).Error())
	case !bytes.Equal(project, workspace):
		return "differs between the project and the workspace"
	}
	return ""
}

// farmOutcome is what provisioning did with one HardLinkFarms entry.
type farmOutcome struct {
	path   string // repo-relative, with "/"
	built  bool
	stats  fslink.FarmStats
	reason string // why no farm was built, when it was skipped
	err    error  // why a farm failed part way; it was removed
}

// applyHardLinkFarms builds a hard-link farm (fslink.HardLinkFarm) of each
// repo-relative project directory in the workspace. A folder the project does
// not have is passed over silently. One that is not a directory, already
// exists in the workspace, has manifests or lockfiles that differ from the
// project's (dependencyLinkMismatch), or sits on another volume than the
// workspace is skipped, and a farm that fails part way is removed: either way
// the workspace gets no folder there and the agent installs its own, so none
// of these abort the spawn. Nothing ever falls back to a shared link or a
// copy. Only a path that is not repo-relative is an error.
func applyHardLinkFarms(projectPath, workspacePath string, farms []string) ([]farmOutcome, error) {
	var outcomes []farmOutcome
	for _, rel := range farms {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		clean, err := safeRelPath(rel)
		if err != nil {
			return outcomes, fmt.Errorf("hardLinkFarm %q: %w", rel, err)
		}
		source := filepath.Join(projectPath, clean)
		info, err := os.Stat(source)
		if err != nil {
			continue
		}
		outcome := farmOutcome{path: filepath.ToSlash(clean)}
		target := filepath.Join(workspacePath, clean)
		switch _, lstatErr := os.Lstat(target); {
		case !info.IsDir():
			outcome.reason = "not a directory in the project"
		case lstatErr == nil:
			outcome.reason = "already present in the workspace"
		default:
			outcome.reason = dependencyLinkMismatch(projectPath, workspacePath, clean)
		}
		if outcome.reason != "" {
			outcomes = append(outcomes, outcome)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			outcome.err = err
			outcomes = append(outcomes, outcome)
			continue
		}
		stats, err := fslink.HardLinkFarm(source, target, fslink.FarmOptions{ProjectRoot: projectPath, WorkspaceRoot: workspacePath})
		switch {
		case errors.Is(err, fslink.ErrDifferentVolume):
			outcome.reason = "different volume: hard links cannot reach the project from this workspace"
		case err != nil:
			outcome.err = err
		default:
			outcome.built, outcome.stats = true, stats
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}
