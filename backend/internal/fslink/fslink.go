// Package fslink links shared paths from a project checkout into a session
// workspace, and takes those links down again without deleting through them.
//
// A session worktree has none of the project's git-ignored dependency folders
// (node_modules, .venv), so a project config can list them as Symlinks and AO
// links each one into every new worktree. How a link is made depends on the
// platform:
//
//   - Unix: os.Symlink, for files and directories alike.
//   - Windows, directory: an NTFS junction (a mount-point reparse point). Unlike
//     a directory symlink it needs no privilege, so it works without Developer
//     Mode or elevation.
//   - Windows, file: os.Symlink when the process may create symlinks. Without
//     that privilege, a hard link when source and workspace share a volume,
//     else a copy of a file up to MaxCopyBytes (a .env, a config file). A hard
//     link shares the file like a symlink does; a copy is a snapshot taken when
//     the workspace is provisioned.
//
// Teardown must never delete through a link into the project's real folder.
// os.RemoveAll unlinks a symlink or junction without following it (covered by
// this package's tests), but other deleters may not, so UnlinkExternal removes
// every link that points outside a workspace before such a delete runs.
package fslink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Method names how Link made a path refer to its source.
type Method string

const (
	// MethodSymlink is a symbolic link (files and directories on Unix, files on
	// Windows when the process holds the symlink privilege).
	MethodSymlink Method = "symlink"
	// MethodJunction is an NTFS junction to a directory (Windows only).
	MethodJunction Method = "junction"
	// MethodHardlink is a hard link to a file on the same volume (Windows only).
	MethodHardlink Method = "hardlink"
	// MethodCopy is a copy of a small file (Windows only, last resort).
	MethodCopy Method = "copy"
)

// MaxCopyBytes bounds the copy fallback for a file link: a .env or config file
// is copied, a large file is refused rather than silently duplicated.
const MaxCopyBytes = 1 << 20

// Link makes target refer to the existing path source and reports how. target
// must not exist; its parent directory must.
func Link(source, target string) (Method, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return linkDir(source, target)
	}
	return linkFile(source, target, info)
}

// IsLink reports whether path itself (not what it points to) is a symbolic
// link or, on Windows, a junction.
func IsLink(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return isLinkInfo(path, info), nil
}

// Unlinked is one link UnlinkExternal removed: Path pointed at Target.
type Unlinked struct {
	Path   string
	Target string
}

// UnlinkExternal removes every symlink or junction under root whose target lies
// outside root, so a deleter that follows links can only ever reach the
// workspace's own files. Links are removed as links; nothing they point to is
// touched. Links inside root are left alone (deleting through them stays inside
// root), the .git entry is not descended into, and a missing root is not an
// error. The removed links are returned so a refused teardown can Restore them.
func UnlinkExternal(root string) ([]Unlinked, error) {
	root = filepath.Clean(root)
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var removed []Unlinked
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == root {
				return walkErr
			}
			// An unreadable subtree holds no link this walk could have seen;
			// the delete that follows reports it if it matters.
			return nil
		}
		if path == root {
			return nil
		}
		skip := func() error {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			return skip()
		}
		// A directory or regular file is no link, so only the rest (a symlink,
		// or a junction, which Windows reports as irregular) is looked at
		// closer: a hard-link farm's thousands of files cost no extra stat.
		if d.Type()&(fs.ModeSymlink|fs.ModeIrregular) == 0 {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil || !isLinkInfo(path, info) {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("fslink: read link %q: %w", path, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if within(root, target) {
			return skip()
		}
		// os.Remove deletes the link entry itself: unlink on Unix, and on
		// Windows DeleteFile, or RemoveDirectory for a directory link, which
		// takes down a junction without touching its target.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("fslink: unlink %q: %w", path, err)
		}
		removed = append(removed, Unlinked{Path: path, Target: target})
		return skip()
	})
	return removed, err
}

// Restore re-creates links UnlinkExternal removed, for a teardown that was
// refused after the links came down. A link whose parent is gone, or whose
// path is taken again, is skipped. Failures are joined, never fatal.
func Restore(links []Unlinked) error {
	var errs []error
	for _, link := range links {
		if _, err := os.Stat(filepath.Dir(link.Path)); err != nil {
			continue
		}
		if _, err := os.Lstat(link.Path); err == nil {
			continue
		}
		if _, err := Link(link.Target, link.Path); err != nil {
			errs = append(errs, fmt.Errorf("fslink: restore %q: %w", link.Path, err))
		}
	}
	return errors.Join(errs...)
}

// within reports whether path is root or lies below it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
