package fslink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/fspath"
)

// A hard-link farm gives a session workspace the project's installed
// node_modules without sharing the folder itself. The farm is a real directory
// tree mirroring the source: every directory is a new directory, every regular
// file is a hard link to the project's file, and every symlink or junction is
// re-created as a link (see relinkTarget for where it points).
//
// Why not a junction to the project's folder: `npm ci` deletes node_modules
// before it installs, and `npm install` rewrites it, so through a junction an
// agent's install wipes or changes the project's real packages for the main
// checkout and every other worktree. In a farm the same install only unlinks
// the farm's entries and writes new files into the farm's own directories; the
// project's files keep their other name and are untouched.
//
// Teardown needs nothing special: os.RemoveAll and `git worktree remove` delete
// the farm's directories and links, never the project's files, and the farm's
// root is a real directory, so UnlinkExternal never mistakes it for a shared
// link.
//
// Residual risk: a hard link is the same file under a second name. A tool that
// writes into an existing file in place (open and truncate, rather than delete
// and create) changes the project's copy too: a postinstall script or
// patch-package editing a dependency file, for example. Installs, `npm ci`,
// `npm rebuild` and node-gyp writing new .node files replace files rather than
// write into them, so they leave the project alone.
//
// A farm is built only on the source's volume (hard links cannot cross one) and
// never falls back to a copy or a shared link: when it cannot be built, the
// workspace simply gets no node_modules and the agent installs its own.

// ErrDifferentVolume reports that a farm's source and target are on different
// volumes, where hard links cannot be made.
var ErrDifferentVolume = errors.New("different volume")

// FarmStats describes a farm HardLinkFarm built.
type FarmStats struct {
	Dirs     int // directories created, the farm's root included
	Files    int // regular files hard-linked
	Links    int // symlinks or junctions re-created
	Skipped  int // dangling links that could not be re-created as links
	Duration time.Duration
}

// FarmOptions tunes how HardLinkFarm re-creates links found in the source.
type FarmOptions struct {
	// ProjectRoot and WorkspaceRoot move an absolute link target that points
	// into the project checkout, outside the source folder, to the same place
	// in the workspace: an npm workspace package linked into node_modules then
	// resolves to the workspace's own copy of that package, as an install run in
	// the workspace would link it. Either empty leaves such targets as they are.
	ProjectRoot, WorkspaceRoot string
}

// farmWorkers bounds the goroutines creating hard links. Directories are made
// by the walk itself, before any file inside them is queued. Creating a hard
// link waits on the file system (and on Windows on its filters, ~1.5-4 ms per
// link measured), not the CPU, so the pool is sized for latency: 10,700 files
// took 22 s with one worker, 11.5 s with 8, 9.3 s with 16 and 10 s with 32 on
// an NTFS hard disk (TestHardLinkFarmTiming).
var farmWorkers = 16

// Seams for tests: fail a link part way, or put the target on another volume.
var (
	farmHardLink   = hardLinkFile
	farmSameVolume = onSameVolume
)

// HardLinkFarm builds a hard-link farm of the directory source at target, which
// must not exist yet; its parent must. Source and target must be on the same
// volume (ErrDifferentVolume otherwise). On any failure the partial farm is
// removed (it holds only new directories and links, so removing it never
// touches the source) and the error is returned: a farm is complete or absent.
func HardLinkFarm(source, target string, opts FarmOptions) (FarmStats, error) {
	started := time.Now()
	source, err := filepath.Abs(source)
	if err != nil {
		return FarmStats{}, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return FarmStats{}, err
	}
	// A project whose node_modules is itself a link is farmed from the folder
	// it points at; links inside it still map back by either name.
	resolved, err := fspath.Resolve(source)
	if err != nil {
		return FarmStats{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return FarmStats{}, err
	}
	if !info.IsDir() {
		return FarmStats{}, fmt.Errorf("fslink: farm source %q is not a directory", source)
	}
	if _, err := os.Lstat(target); err == nil {
		return FarmStats{}, fmt.Errorf("fslink: farm target %q: %w", target, fs.ErrExist)
	}
	same, err := farmSameVolume(resolved, filepath.Dir(target))
	if err != nil {
		return FarmStats{}, err
	}
	if !same {
		return FarmStats{}, ErrDifferentVolume
	}
	if err := os.Mkdir(target, 0o750); err != nil {
		return FarmStats{}, err
	}
	f := &farm{source: resolved, aliases: []string{source}, target: target, workspace: opts.WorkspaceRoot}
	if opts.ProjectRoot != "" && opts.WorkspaceRoot != "" {
		f.projectRoots = []string{opts.ProjectRoot}
		if root, err := fspath.Resolve(opts.ProjectRoot); err == nil {
			f.projectRoots = append(f.projectRoots, root)
		}
	}
	stats, err := f.build()
	if err != nil {
		if removeErr := os.RemoveAll(target); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("fslink: remove partial farm %q: %w", target, removeErr))
		}
		return FarmStats{}, err
	}
	stats.Duration = time.Since(started)
	return stats, nil
}

type farm struct {
	source       string
	aliases      []string // other names of source (the unresolved path)
	target       string
	projectRoots []string // FarmOptions.ProjectRoot, also resolved
	workspace    string
}

type pendingLink struct {
	from, to string
	info     fs.FileInfo
}

var errFarmAborted = errors.New("fslink: farm aborted")

func (f *farm) build() (FarmStats, error) {
	stats := FarmStats{Dirs: 1}
	var (
		failed   atomic.Bool
		errOnce  sync.Once
		firstErr error
		files    atomic.Int64
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
		failed.Store(true)
	}
	jobs := make(chan [2]string, 512)
	for range farmWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if failed.Load() {
					continue
				}
				if err := farmHardLink(job[0], job[1]); err != nil {
					fail(fmt.Errorf("fslink: hard link %q: %w", job[0], err))
					continue
				}
				files.Add(1)
			}
		}()
	}
	// Links are made after every directory and file exists, so a link to a
	// path inside the farm is created against its real target.
	var links []pendingLink
	walkErr := filepath.WalkDir(f.source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if failed.Load() {
			return errFarmAborted
		}
		if path == f.source {
			return nil
		}
		rel, err := filepath.Rel(f.source, path)
		if err != nil {
			return err
		}
		to := filepath.Join(f.target, rel)
		switch kind := d.Type(); {
		case d.IsDir():
			if err := os.Mkdir(to, 0o750); err != nil {
				return err
			}
			stats.Dirs++
		case kind.IsRegular():
			jobs <- [2]string{path, to}
		default:
			// A symlink, a junction (irregular on Windows), or something else.
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if isLinkInfo(path, info) {
				links = append(links, pendingLink{from: path, to: to, info: info})
				return nil
			}
			if !info.Mode().IsRegular() && info.Mode()&fs.ModeIrregular == 0 {
				return fmt.Errorf("fslink: cannot farm %q: unsupported file type %v", path, info.Mode().Type())
			}
			// Any other reparse point (a dedup or cloud file) is linked as a
			// file; if the volume refuses, the farm fails rather than copies.
			jobs <- [2]string{path, to}
		}
		return nil
	})
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return FarmStats{}, firstErr
	}
	if walkErr != nil {
		return FarmStats{}, walkErr
	}
	stats.Files = int(files.Load())
	for _, link := range links {
		made, err := f.relink(link)
		if err != nil {
			return FarmStats{}, fmt.Errorf("fslink: re-create link %q: %w", link.from, err)
		}
		if made {
			stats.Links++
		} else {
			stats.Skipped++
		}
	}
	return stats, nil
}

// relinkTarget is where the farm's copy of a link points. A relative target is
// kept: the farm mirrors the source layout, so it resolves to the farm's own
// entry (or, leaving the folder, to the same place relative to the workspace).
// An absolute target inside the source folder moves to the same place in the
// farm (pnpm's .pnpm store links, for example), one inside the project
// checkout moves to the workspace (FarmOptions), and any other absolute target
// is kept as it is (a global store or an `npm link`ed folder): the farm's link
// then shares that folder exactly as the project's link does.
func (f *farm) relinkTarget(target string) string {
	if !filepath.IsAbs(target) {
		return target
	}
	for _, source := range append([]string{f.source}, f.aliases...) {
		if within(source, target) {
			rel, err := filepath.Rel(source, filepath.Clean(target))
			if err == nil {
				return filepath.Join(f.target, rel)
			}
		}
	}
	for _, root := range f.projectRoots {
		if within(root, target) {
			if rel, err := filepath.Rel(root, filepath.Clean(target)); err == nil {
				return filepath.Join(f.workspace, rel)
			}
		}
	}
	return target
}

// relink re-creates one link in the farm and reports whether it did; false
// means a dangling link that the platform could not re-create as a link.
func (f *farm) relink(link pendingLink) (bool, error) {
	target, err := os.Readlink(link.from)
	if err != nil {
		return false, err
	}
	return makeFarmLink(link.from, link.to, f.relinkTarget(target), link.info)
}
