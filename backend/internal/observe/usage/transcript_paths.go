package usage

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/fspath"
)

// transcriptPaths canonicalizes many transcript paths at once, as one inventory
// refresh does for every watched source and then again in the watcher's
// Rebuild. canonicalTranscriptPath resolves each path from the root, and on
// Windows EvalSymlinks stats and lists every component: ~500 sources cost
// 2.5-5 s of CPU and ~100-190k filesystem calls per refresh, every 3 minutes.
// Here each folder is resolved and listed once; a file is its folder's
// resolution joined with its name as the listing spells it. A file that is
// itself a link, or whose folder cannot be resolved, still goes through
// canonicalTranscriptPath, so every result matches it.
type transcriptPaths struct {
	dirs map[string]transcriptDir
}

type transcriptDir struct {
	resolved string
	ok       bool
	entries  map[string]fs.DirEntry
}

type transcriptPathsKey struct{}

// withTranscriptPaths makes canonicalTranscriptPathIn share folder resolutions
// and listings under the returned context. It is not synchronized: use it from
// one goroutine, for one refresh.
func withTranscriptPaths(ctx context.Context) context.Context {
	return context.WithValue(ctx, transcriptPathsKey{}, &transcriptPaths{dirs: map[string]transcriptDir{}})
}

// canonicalTranscriptPathIn is canonicalTranscriptPath, through the context's
// shared folder resolutions when it carries them.
func canonicalTranscriptPathIn(ctx context.Context, path string) string {
	paths, _ := ctx.Value(transcriptPathsKey{}).(*transcriptPaths)
	if paths == nil {
		return canonicalTranscriptPath(path)
	}
	return paths.canonical(path)
}

func (p *transcriptPaths) canonical(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	dir := p.dir(filepath.Dir(path))
	if !dir.ok {
		return canonicalTranscriptPath(path)
	}
	base := filepath.Base(path)
	entry, found := dir.entries[transcriptNameKey(base)]
	if !found {
		// As canonicalTranscriptPath does for a file that no longer exists.
		return filepath.Join(dir.resolved, base)
	}
	if entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return canonicalTranscriptPath(path)
	}
	return filepath.Join(dir.resolved, entry.Name())
}

func (p *transcriptPaths) dir(path string) transcriptDir {
	if dir, ok := p.dirs[path]; ok {
		return dir
	}
	var dir transcriptDir
	if resolved, err := fspath.Resolve(path); err == nil {
		if entries, err := os.ReadDir(resolved); err == nil {
			dir = transcriptDir{resolved: resolved, ok: true, entries: make(map[string]fs.DirEntry, len(entries))}
			for _, entry := range entries {
				dir.entries[transcriptNameKey(entry.Name())] = entry
			}
		}
	}
	p.dirs[path] = dir
	return dir
}

// transcriptNameKey folds case on Windows, where EvalSymlinks returns each
// name as the folder spells it; elsewhere it keeps the caller's spelling, and
// so does a lookup that misses.
func transcriptNameKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}
