package fslink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// farmProject lays out a project checkout with an installed node_modules: a
// few packages, a .bin folder of plain shim files, and returns the project root
// and its node_modules.
func farmProject(t *testing.T) (string, string) {
	t.Helper()
	project := filepath.Join(t.TempDir(), "project")
	deps := filepath.Join(project, "node_modules")
	files := map[string]string{
		"package.json":                           `{"name":"app"}`,
		"node_modules/.package-lock.json":        "{}",
		"node_modules/react/package.json":        `{"name":"react"}`,
		"node_modules/react/index.js":            "module.exports = 'react'\n",
		"node_modules/react/cjs/react.min.js":    "min\n",
		"node_modules/@scope/tool/bin/tool.js":   "#!/usr/bin/env node\n",
		"node_modules/.bin/tool":                 "#!/bin/sh\n",
		"node_modules/.bin/tool.cmd":             "@node tool.js %*\r\n",
		"node_modules/.bin/tool.ps1":             "node tool.js $args\r\n",
		"packages/shared/package.json":           `{"name":"shared"}`,
		"packages/shared/index.js":               "module.exports = 'project shared'\n",
		"node_modules/empty-dir-sibling/keep.js": "",
	}
	for rel, data := range files {
		writeTree(t, filepath.Join(project, rel), data)
	}
	if err := os.MkdirAll(filepath.Join(deps, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	return project, deps
}

func writeTree(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// regularFiles lists the regular files under root, relative to it.
func regularFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// assertHardLinked fails unless every regular file of the source is the same
// file as its farm twin and has exactly the two names: a copy, which has one
// name and a different file ID, fails here.
func assertHardLinked(t *testing.T, source, farm string) int {
	t.Helper()
	files := regularFiles(t, source)
	if len(files) == 0 {
		t.Fatal("no files to compare")
	}
	for _, rel := range files {
		from, to := filepath.Join(source, rel), filepath.Join(farm, rel)
		info, err := os.Lstat(to)
		if err != nil {
			t.Fatalf("farm is missing %s: %v", rel, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("farm %s is %v, want a regular file (hard link)", rel, info.Mode().Type())
		}
		if !sameFile(t, from, to) {
			t.Fatalf("farm %s is not the project's file (copied, not hard-linked)", rel)
		}
		if n := linkCount(t, to); n != 2 {
			t.Fatalf("farm %s has %d names, want 2 (the project's and the farm's)", rel, n)
		}
	}
	return len(files)
}

func TestHardLinkFarmMirrorsNodeModulesWithHardLinks(t *testing.T) {
	project, deps := farmProject(t)
	workspace := filepath.Join(t.TempDir(), "worktree")
	writeTree(t, filepath.Join(workspace, "packages", "shared", "index.js"), "module.exports = 'workspace shared'\n")
	external := filepath.Join(t.TempDir(), "global-store", "linked")
	writeTree(t, filepath.Join(external, "index.js"), "global\n")
	// Directory links as package managers lay them out: a pnpm-style link to a
	// folder inside node_modules, an npm workspace link to the project's own
	// package, and an `npm link` to a folder outside the project.
	linkDirOrSkip(t, filepath.Join(deps, "react"), filepath.Join(deps, "react-alias"))
	linkDirOrSkip(t, filepath.Join(project, "packages", "shared"), filepath.Join(deps, "shared"))
	linkDirOrSkip(t, external, filepath.Join(deps, "linked"))
	if runtime.GOOS != "windows" {
		// A relative link stays relative, so it resolves inside the farm.
		if err := os.Symlink(filepath.Join("..", "@scope", "tool", "bin", "tool.js"), filepath.Join(deps, ".bin", "tool-rel")); err != nil {
			t.Fatal(err)
		}
	}

	target := filepath.Join(workspace, "node_modules")
	stats, err := HardLinkFarm(deps, target, FarmOptions{ProjectRoot: project, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("HardLinkFarm: %v", err)
	}
	files := assertHardLinked(t, deps, target)
	if stats.Files != files {
		t.Fatalf("stats.Files = %d, want %d", stats.Files, files)
	}
	wantLinks := 3
	if runtime.GOOS != "windows" {
		wantLinks = 4
	}
	if stats.Links != wantLinks || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want %d links", stats, wantLinks)
	}
	// The farm and every folder in it are real directories, not links.
	for _, dir := range []string{"", "react", "react/cjs", "@scope/tool/bin", ".bin", "empty"} {
		path := filepath.Join(target, filepath.FromSlash(dir))
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			t.Fatalf("farm dir %q = %v, %v; want a real directory", dir, info, err)
		}
		if isLink, _ := IsLink(path); isLink {
			t.Fatalf("farm dir %q is a link", dir)
		}
	}
	// Each link points where an install in the workspace would point it.
	for name, want := range map[string]string{
		"react-alias": filepath.Join(target, "react"),
		"shared":      filepath.Join(workspace, "packages", "shared"),
		"linked":      external,
	} {
		path := filepath.Join(target, name)
		if isLink, err := IsLink(path); err != nil || !isLink {
			t.Fatalf("farm %s is not a link: %v, %v", name, isLink, err)
		}
		got, err := os.Readlink(path)
		if err != nil || filepath.Clean(got) != want {
			t.Fatalf("farm %s -> %q, %v; want %q", name, got, err, want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(target, "shared", "index.js")); err != nil || string(data) != "module.exports = 'workspace shared'\n" {
		t.Fatalf("workspace package through the farm = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		if got, err := os.Readlink(filepath.Join(target, ".bin", "tool-rel")); err != nil || filepath.IsAbs(got) {
			t.Fatalf("relative link became %q, %v", got, err)
		}
	}
}

// Deleting the farm (teardown) or replacing it (`npm ci`, `npm install`)
// removes only the farm's names; the project's files keep theirs, unchanged.
func TestHardLinkFarmTeardownAndReinstallKeepProjectFiles(t *testing.T) {
	_, deps := farmProject(t)
	before := map[string]string{}
	for _, rel := range regularFiles(t, deps) {
		data, err := os.ReadFile(filepath.Join(deps, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = string(data)
	}
	assertProject := func(t *testing.T, step string) {
		t.Helper()
		for rel, want := range before {
			path := filepath.Join(deps, rel)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != want {
				t.Fatalf("%s: project %s = %q, %v; want %q", step, rel, data, err, want)
			}
		}
	}
	workspace := t.TempDir()
	target := filepath.Join(workspace, "node_modules")
	build := func(t *testing.T) {
		t.Helper()
		if _, err := HardLinkFarm(deps, target, FarmOptions{}); err != nil {
			t.Fatalf("HardLinkFarm: %v", err)
		}
	}

	// Teardown: os.RemoveAll of the farm.
	build(t)
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	assertProject(t, "after RemoveAll")
	for rel := range before {
		if n := linkCount(t, filepath.Join(deps, rel)); n != 1 {
			t.Fatalf("project %s has %d names after the farm was removed, want 1", rel, n)
		}
	}

	// `npm ci`: delete node_modules, then write a fresh install in its place.
	build(t)
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	for rel := range before {
		writeTree(t, filepath.Join(target, rel), "reinstalled by npm ci\n")
	}
	assertProject(t, "after npm ci")

	// `npm install` updating one package: unlink its files and write new ones.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	build(t)
	index := filepath.Join(target, "react", "index.js")
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	writeTree(t, index, "module.exports = 'react 19'\n")
	if err := os.RemoveAll(filepath.Join(target, "react", "cjs")); err != nil {
		t.Fatal(err)
	}
	assertProject(t, "after npm install")

	// The documented residual risk: writing into a farm file in place (open
	// and truncate, as a postinstall patch might) changes the project's file,
	// because it is the same file.
	shared := filepath.Join(target, "@scope", "tool", "bin", "tool.js")
	if err := os.WriteFile(shared, []byte("patched in place\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(deps, "@scope", "tool", "bin", "tool.js")); string(data) != "patched in place\n" {
		t.Fatalf("in-place write did not reach the shared file: %q", data)
	}
}

// A link that fails part way removes the whole farm and leaves the project
// alone; nothing falls back to copying.
func TestHardLinkFarmFailureRemovesPartialFarm(t *testing.T) {
	_, deps := farmProject(t)
	previous := farmHardLink
	var calls atomic.Int32
	farmHardLink = func(existing, link string) error {
		if calls.Add(1) == 4 {
			return errors.New("injected link failure")
		}
		return previous(existing, link)
	}
	t.Cleanup(func() { farmHardLink = previous })

	target := filepath.Join(t.TempDir(), "node_modules")
	if _, err := HardLinkFarm(deps, target, FarmOptions{}); err == nil || !strings.Contains(err.Error(), "injected link failure") {
		t.Fatalf("HardLinkFarm error = %v, want the injected failure", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial farm left behind: %v", err)
	}
	for _, rel := range regularFiles(t, deps) {
		if n := linkCount(t, filepath.Join(deps, rel)); n != 1 {
			t.Fatalf("project %s has %d names after a failed farm, want 1", rel, n)
		}
	}
}

func TestHardLinkFarmRefusesOtherVolumeAndExistingTarget(t *testing.T) {
	_, deps := farmProject(t)
	previous := farmSameVolume
	farmSameVolume = func(string, string) (bool, error) { return false, nil }
	target := filepath.Join(t.TempDir(), "node_modules")
	_, err := HardLinkFarm(deps, target, FarmOptions{})
	farmSameVolume = previous
	if !errors.Is(err, ErrDifferentVolume) {
		t.Fatalf("HardLinkFarm across volumes = %v, want ErrDifferentVolume", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("farm created across volumes: %v", err)
	}

	if same, err := onSameVolume(deps, t.TempDir()); err != nil {
		t.Fatalf("onSameVolume: %v", err)
	} else if !same && runtime.GOOS == "windows" && strings.EqualFold(filepath.VolumeName(deps), filepath.VolumeName(os.TempDir())) {
		t.Fatal("two temp directories reported on different volumes")
	}

	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := HardLinkFarm(deps, target, FarmOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("HardLinkFarm onto an existing path = %v, want ErrExist", err)
	}
}

// UnlinkExternal (run before `git worktree remove`) leaves the farm in place:
// its root is a real directory and its files are not links. Only a link in it
// pointing outside the workspace is taken down.
func TestUnlinkExternalLeavesHardLinkFarm(t *testing.T) {
	_, deps := farmProject(t)
	external := filepath.Join(t.TempDir(), "global-store")
	writeTree(t, filepath.Join(external, "index.js"), "global\n")
	linkDirOrSkip(t, filepath.Join(deps, "react"), filepath.Join(deps, "react-alias"))
	linkDirOrSkip(t, external, filepath.Join(deps, "linked"))
	workspace := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "node_modules")
	if _, err := HardLinkFarm(deps, target, FarmOptions{}); err != nil {
		t.Fatalf("HardLinkFarm: %v", err)
	}
	removed, err := UnlinkExternal(workspace)
	if err != nil {
		t.Fatalf("UnlinkExternal: %v", err)
	}
	if len(removed) != 1 || removed[0].Path != filepath.Join(target, "linked") {
		t.Fatalf("removed = %+v, want only the link to the external store", removed)
	}
	if isLink, err := IsLink(filepath.Join(target, "react-alias")); err != nil || !isLink {
		t.Fatalf("link inside the farm was removed: %v, %v", isLink, err)
	}
	assertHardLinked(t, filepath.Join(deps, "react"), filepath.Join(target, "react"))
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(external, "index.js")); err != nil || string(data) != "global\n" {
		t.Fatalf("external store after teardown = %q, %v", data, err)
	}
}

// TestHardLinkFarmTiming builds a farm of a few thousand files with one worker
// and with the default pool, and logs both. Set AO_FARM_TIMING_FILES to size
// it (the user's frontend has ~10.7k files), AO_FARM_TIMING_WORKERS to compare
// pool sizes ("1,4,8,16"), or AO_FARM_TIMING_DIR to time it on another drive.
func TestHardLinkFarmTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("timing only")
	}
	count := 3000
	if value := os.Getenv("AO_FARM_TIMING_FILES"); value != "" {
		if _, err := fmt.Sscan(value, &count); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	if dir := os.Getenv("AO_FARM_TIMING_DIR"); dir != "" {
		var err error
		if base, err = os.MkdirTemp(dir, "farm-timing-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(base) })
	}
	deps := filepath.Join(base, "project", "node_modules")
	for i := range count {
		// ~15 files per package, a couple of folders deep, like node_modules.
		pkg := fmt.Sprintf("pkg-%04d", i/15)
		writeTree(t, filepath.Join(deps, pkg, "lib", fmt.Sprintf("file-%d.js", i)), "module.exports = 1\n")
	}
	run := func(workers int) (FarmStats, time.Duration) {
		previous := farmWorkers
		farmWorkers = workers
		defer func() { farmWorkers = previous }()
		target := filepath.Join(base, fmt.Sprintf("worktree-%d", workers), "node_modules")
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		stats, err := HardLinkFarm(deps, target, FarmOptions{})
		if err != nil {
			t.Fatalf("HardLinkFarm(%d workers): %v", workers, err)
		}
		removeStarted := time.Now()
		if err := os.RemoveAll(filepath.Dir(target)); err != nil {
			t.Fatal(err)
		}
		return stats, time.Since(removeStarted)
	}
	workerCounts := []int{1, farmWorkers}
	if value := os.Getenv("AO_FARM_TIMING_WORKERS"); value != "" {
		workerCounts = nil
		for _, field := range strings.Split(value, ",") {
			var workers int
			if _, err := fmt.Sscan(field, &workers); err != nil {
				t.Fatal(err)
			}
			workerCounts = append(workerCounts, workers)
		}
	}
	for _, workers := range workerCounts {
		stats, removal := run(workers)
		if stats.Files != count {
			t.Fatalf("farmed %d files, want %d", stats.Files, count)
		}
		t.Logf("%d files, %d dirs, %d workers: farm %v, removal %v", stats.Files, stats.Dirs, workers, stats.Duration, removal)
	}
}
