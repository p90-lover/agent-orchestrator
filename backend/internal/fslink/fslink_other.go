//go:build !windows

package fslink

import (
	"io/fs"
	"os"
)

// linkDir symlinks a directory; Unix needs no privilege for it.
func linkDir(source, target string) (Method, error) {
	if err := os.Symlink(source, target); err != nil {
		return "", err
	}
	return MethodSymlink, nil
}

// linkFile symlinks a file.
func linkFile(source, target string, _ fs.FileInfo) (Method, error) {
	if err := os.Symlink(source, target); err != nil {
		return "", err
	}
	return MethodSymlink, nil
}

func isLinkInfo(_ string, info fs.FileInfo) bool {
	return info.Mode()&fs.ModeSymlink != 0
}
