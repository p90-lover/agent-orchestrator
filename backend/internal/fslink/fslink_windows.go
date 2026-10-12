//go:build windows

package fslink

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Seams for tests: the symlink privilege and hard-link support depend on the
// host, so the fallbacks are driven through these instead.
var (
	symlinkFile = os.Symlink
	hardLink    = os.Link
)

// linkDir makes target an NTFS junction to source. A junction is a
// mount-point reparse point on an empty directory: creating one needs only
// write access to the parent, not SeCreateSymbolicLinkPrivilege, so it works
// without Developer Mode or elevation. It points at an absolute local path,
// which source always is here.
func linkDir(source, target string) (Method, error) {
	abs, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(target, 0o750); err != nil {
		return "", err
	}
	if err := setMountPoint(target, abs); err != nil {
		// Leave nothing half-made: the directory is still empty.
		_ = os.Remove(target)
		return "", fmt.Errorf("create junction: %w", err)
	}
	return MethodJunction, nil
}

func setMountPoint(dir, target string) error {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	data := winio.EncodeReparsePoint(&winio.ReparsePoint{Target: target, IsMountPoint: true})
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, &returned, nil) //nolint:gosec // reparse buffers are far below 4 GiB
}

// linkFile symlinks a file when the process may; otherwise it hard-links on the
// same volume, and copies a small file as the last resort.
func linkFile(source, target string, info fs.FileInfo) (Method, error) {
	err := symlinkFile(source, target)
	if err == nil {
		return MethodSymlink, nil
	}
	if !errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return "", err
	}
	if sameVolume(source, target) {
		if hardLink(source, target) == nil {
			return MethodHardlink, nil
		}
	}
	if info.Size() > MaxCopyBytes {
		return "", fmt.Errorf("cannot link %q: no symlink privilege, no hard link, and %d bytes is too large to copy", source, info.Size())
	}
	if err := copyFile(source, target, info.Mode().Perm()); err != nil {
		return "", err
	}
	return MethodCopy, nil
}

func sameVolume(a, b string) bool {
	va, vb := filepath.VolumeName(a), filepath.VolumeName(b)
	return va != "" && strings.EqualFold(va, vb)
}

func copyFile(source, target string, perm fs.FileMode) (err error) {
	in, err := os.Open(source) //nolint:gosec // source is the project's own repo-relative path
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // target is inside the AO-managed workspace
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	_, err = io.Copy(out, io.LimitReader(in, MaxCopyBytes+1))
	return err
}

// isLinkInfo reports a symlink, or a reparse point os.Readlink can resolve,
// which on Windows means a junction (mount point). Other reparse points (dedup,
// cloud placeholders) are ordinary files to this package.
func isLinkInfo(path string, info fs.FileInfo) bool {
	if info.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attrs.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return false
	}
	_, err := os.Readlink(path)
	return err == nil
}
