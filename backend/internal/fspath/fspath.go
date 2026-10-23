// Package fspath resolves filesystem paths to the location they name.
//
// Since Go 1.23, os.Lstat reports a Windows junction (mount point) as an
// irregular file rather than a symlink, and filepath.EvalSymlinks then neither
// follows it nor walks through it: EvalSymlinks of the junction returns it
// unchanged, and EvalSymlinks of any path below it fails with "The system
// cannot find the path specified". Moving a data folder elsewhere and leaving
// a junction behind (C:\Users\me\.ao -> G:\AppData\.ao) therefore made every
// transcript beneath it look missing, and a junction root look unrelated to
// the resolved paths beneath it.
package fspath

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Resolve is filepath.EvalSymlinks, except that on Windows a junction is
// followed too: a path that EvalSymlinks cannot resolve but that exists, or
// one that EvalSymlinks returns still ending in a junction, is resolved through
// an open handle. Other paths keep exactly EvalSymlinks' result, so values
// stored from it still compare equal. When both fail, Resolve returns
// EvalSymlinks' error.
func Resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if !resolveThroughHandle {
		return resolved, err
	}
	if err == nil {
		if info, lstatErr := os.Lstat(resolved); lstatErr != nil || info.Mode()&fs.ModeIrregular == 0 {
			return resolved, nil
		}
		if final, finalErr := FinalPath(resolved); finalErr == nil {
			return final, nil
		}
		return resolved, nil
	}
	if final, finalErr := FinalPath(path); finalErr == nil {
		return final, nil
	}
	return "", err
}
