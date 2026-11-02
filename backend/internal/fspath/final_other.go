//go:build !windows

package fspath

import "path/filepath"

const resolveThroughHandle = false

// FinalPath is filepath.EvalSymlinks: outside Windows it already follows every
// kind of link.
func FinalPath(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
