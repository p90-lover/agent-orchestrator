package session

import "github.com/aoagents/agent-orchestrator/backend/internal/fspath"

// resolvedFilesystemPath is the path's final location, through every symlink
// and, on Windows, every junction.
func resolvedFilesystemPath(path string) (string, error) {
	return fspath.FinalPath(path)
}
