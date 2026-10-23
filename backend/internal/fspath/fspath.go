// Package fspath resolves filesystem paths to the location they name.
//
// Since Go 1.23, os.Lstat reports a Windows junction (mount point) as an
// irregular file rather than a symlink, and filepath.EvalSymlinks then neither
// follows it nor walks through it: EvalSymlinks of the junction returns it
// unchanged, and EvalSymlinks of any path below it fails with "The system
// cannot find the path specified". Moving a data folder elsewhere and leaving
// a junction behind (C:\Users\me\.ao -> G:\AppData\.ao) therefore made every
// transcript beneath it look missing.
package fspath

import "path/filepath"

// Resolve is filepath.EvalSymlinks, except that on Windows a path that
// EvalSymlinks cannot resolve but that exists is resolved through an open
// handle, which follows junctions as well as symlinks. Paths EvalSymlinks
// handles keep exactly its result, so values stored from it still compare
// equal. When both fail, Resolve returns EvalSymlinks' error.
func Resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil || !resolveThroughHandle {
		return resolved, err
	}
	if final, finalErr := FinalPath(path); finalErr == nil {
		return final, nil
	}
	return "", err
}
