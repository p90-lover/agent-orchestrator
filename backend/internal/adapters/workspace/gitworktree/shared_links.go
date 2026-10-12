package gitworktree

import (
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/fslink"
)

// A project's Symlinks config links shared folders (node_modules, .venv) from
// the project checkout into every session worktree: junctions on Windows,
// symlinks elsewhere. Teardown must remove those links, never what they point
// at.
//
// os.RemoveAll unlinks a link without following it, so the discard path (move
// aside, RemoveAll in the background) and the RemoveAll backstops are safe as
// they are. `git worktree remove` is not trusted with them: Git for Windows has
// deleted through node_modules junctions before. Every git-driven removal
// therefore first takes down each link that points outside the worktree, and a
// refused removal puts them back so the preserved worktree still works.

// unlinkSharedLinks removes every link under path that points outside it. A
// failure aborts the teardown: deleting the tree with an external link still in
// place could reach the project's real files.
func unlinkSharedLinks(path string) ([]fslink.Unlinked, error) {
	removed, err := fslink.UnlinkExternal(path)
	if err != nil {
		return removed, fmt.Errorf("gitworktree: unlink shared links in %q before removal: %w", path, err)
	}
	return removed, nil
}

// restoreSharedLinks re-creates links unlinkSharedLinks removed, for a teardown
// that left the worktree in place. Failures are logged: the worktree is still
// intact and a missing dependency link only costs a reinstall.
func (w *Workspace) restoreSharedLinks(links []fslink.Unlinked) {
	if len(links) == 0 {
		return
	}
	if err := fslink.Restore(links); err != nil {
		w.logf("gitworktree: could not restore shared links: %v", err)
	}
}
