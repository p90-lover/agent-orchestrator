//go:build windows

package fslink

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/aoagents/agent-orchestrator/backend/internal/fspath"
)

// symbolicLinkFlagAllowUnprivilegedCreate lets CreateSymbolicLink work without
// the symlink privilege where Developer Mode allows it.
const symbolicLinkFlagAllowUnprivilegedCreate = 0x2

// hardLinkFile makes link a second name for the file existing (CreateHardLinkW).
func hardLinkFile(existing, link string) error {
	linkName, err := windows.UTF16PtrFromString(longPath(link))
	if err != nil {
		return err
	}
	existingName, err := windows.UTF16PtrFromString(longPath(existing))
	if err != nil {
		return err
	}
	return windows.CreateHardLink(linkName, existingName, 0)
}

// longPath gives an absolute path the \\?\ prefix so calls made straight to the
// Windows API reach deep node_modules paths past MAX_PATH, as package os does
// for its own calls. A relative or already prefixed path is returned as is.
func longPath(path string) string {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + path[2:]
	}
	return `\\?\` + path
}

// onSameVolume compares the volume serial numbers of two existing paths.
func onSameVolume(a, b string) (bool, error) {
	va, err := volumeSerial(a)
	if err != nil {
		return false, err
	}
	vb, err := volumeSerial(b)
	if err != nil {
		return false, err
	}
	return va == vb, nil
}

func volumeSerial(path string) (uint32, error) {
	name, err := windows.UTF16PtrFromString(longPath(path))
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var data windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &data); err != nil {
		return 0, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	return data.VolumeSerialNumber, nil
}

// makeFarmLink re-creates the link from (described by info) at to, pointing at
// target. npm, pnpm and yarn link directories inside node_modules with
// junctions on Windows (their .bin shims are plain .cmd/.ps1/sh files, which
// the farm hard-links), so a junction stays a junction, to an absolute path. A
// symlink stays a symlink of the same kind, file or directory; without the
// symlink privilege a directory symlink becomes a junction to the same place
// and a file symlink a hard link to the file it resolves to. A dangling file
// symlink that cannot be re-created as one is skipped (false).
func makeFarmLink(from, to, target string, info fs.FileInfo) (bool, error) {
	absolute := target
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(filepath.Dir(to), target)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		// isLinkInfo accepted it, so it is a junction (mount point).
		if _, err := linkDir(absolute, to); err != nil {
			return false, err
		}
		return true, nil
	}
	isDir := false
	if attrs, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		isDir = attrs.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	}
	linkName, err := windows.UTF16PtrFromString(longPath(to))
	if err != nil {
		return false, err
	}
	targetName, err := windows.UTF16PtrFromString(filepath.FromSlash(target))
	if err != nil {
		return false, err
	}
	flags := uint32(symbolicLinkFlagAllowUnprivilegedCreate)
	if isDir {
		flags |= windows.SYMBOLIC_LINK_FLAG_DIRECTORY
	}
	err = windows.CreateSymbolicLink(linkName, targetName, flags)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return false, &os.LinkError{Op: "symlink", Old: target, New: to, Err: err}
	}
	if isDir {
		if _, err := linkDir(absolute, to); err != nil {
			return false, err
		}
		return true, nil
	}
	resolved, err := fspath.Resolve(from)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := hardLinkFile(resolved, to); err != nil {
		return false, &os.LinkError{Op: "link", Old: resolved, New: to, Err: err}
	}
	return true, nil
}
