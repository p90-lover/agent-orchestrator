package usage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// discoveryPass remembers folder listings and symlink resolutions for one
// discovery pass. Every binding globs the same provider folders (Codex's
// sessions/*/*/*, every Claude project folder) and validates the same roots and
// sibling transcripts, and on Windows filepath.Glob and filepath.EvalSymlinks
// redo all of it per binding (EvalSymlinks stats and lists every component of
// the path): one pass over 64 bindings made ~94k filesystem calls and spent
// ~2.7 s of CPU. A pass lasts seconds and the transcript watcher reconciles
// anything created meanwhile, so what the pass saw first is fresh enough.
type discoveryPass struct {
	listings map[string][]string
	resolved map[string]resolvedPath
}

type resolvedPath struct {
	path string
	err  error
}

type discoveryPassKey struct{}

// withDiscoveryPass makes globs and symlink resolution under the returned
// context share their results. The pass is not synchronized: only one
// goroutine's discovery loop may use it.
func withDiscoveryPass(ctx context.Context) context.Context {
	return context.WithValue(ctx, discoveryPassKey{}, &discoveryPass{
		listings: map[string][]string{},
		resolved: map[string]resolvedPath{},
	})
}

func discoveryPassFrom(ctx context.Context) *discoveryPass {
	pass, _ := ctx.Value(discoveryPassKey{}).(*discoveryPass)
	return pass
}

// discoveryGlob is filepath.Glob, reading folders through the pass's listings
// when ctx carries a discovery pass.
func discoveryGlob(ctx context.Context, pattern string) ([]string, error) {
	pass := discoveryPassFrom(ctx)
	if pass == nil {
		return filepath.Glob(pattern)
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	return pass.glob(pattern), nil
}

// discoveryEvalSymlinks is filepath.EvalSymlinks, remembered for the pass when
// ctx carries one.
func discoveryEvalSymlinks(ctx context.Context, path string) (string, error) {
	pass := discoveryPassFrom(ctx)
	if pass == nil {
		return filepath.EvalSymlinks(path)
	}
	if result, ok := pass.resolved[path]; ok {
		return result.path, result.err
	}
	resolved, err := filepath.EvalSymlinks(path)
	pass.resolved[path] = resolvedPath{path: resolved, err: err}
	return resolved, err
}

func (p *discoveryPass) glob(pattern string) []string {
	if !globHasMeta(pattern) {
		if _, err := os.Lstat(pattern); err != nil {
			return nil
		}
		return []string{pattern}
	}
	dir, file := filepath.Split(pattern)
	dir = filepath.Clean(dir)
	if !globHasMeta(dir) {
		return p.match(dir, file, nil)
	}
	var matches []string
	for _, parent := range p.glob(dir) {
		matches = p.match(parent, file, matches)
	}
	return matches
}

func (p *discoveryPass) match(dir, pattern string, matches []string) []string {
	names, ok := p.listings[dir]
	if !ok {
		if folder, err := os.Open(dir); err == nil { //nolint:gosec // dir comes from a configured provider root.
			names, _ = folder.Readdirnames(-1)
			_ = folder.Close()
			sort.Strings(names)
		}
		p.listings[dir] = names
	}
	for _, name := range names {
		if matched, _ := filepath.Match(pattern, name); matched {
			matches = append(matches, filepath.Join(dir, name))
		}
	}
	return matches
}

// globHasMeta matches filepath's hasMeta: on Windows a backslash is a path
// separator, not an escape.
func globHasMeta(path string) bool {
	magic := `*?[`
	if filepath.Separator != '\\' {
		magic = `*?[\`
	}
	return strings.ContainsAny(path, magic)
}
