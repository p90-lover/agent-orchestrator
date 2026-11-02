package session

import (
	"os"
	"sync"
)

// workspaceFileFacts remembers what reading a workspace file told: whether its
// first 8 KiB look binary, and how many lines it has. Every workspace listing
// asked both again for every file, reading each one: an Overview panel polling
// a project's changes every few seconds kept the daemon reading untracked
// backups and caches in full, without end. A file whose size and modification
// time are unchanged gives the same answers, so it is not read again.
//
// The cache is bounded; when full it starts over, which only costs the reads
// it saved.
type workspaceFileFactsCache struct {
	mu      sync.Mutex
	entries map[string]workspaceFileFactsEntry
	limit   int
}

type workspaceFileFactsEntry struct {
	size    int64
	modTime int64

	binaryKnown bool
	binary      bool

	linesKnown bool
	lines      int
	linesOK    bool
}

const workspaceFileFactsLimit = 16384

var workspaceFileFacts = newWorkspaceFileFactsCache(workspaceFileFactsLimit)

func newWorkspaceFileFactsCache(limit int) *workspaceFileFactsCache {
	return &workspaceFileFactsCache{entries: map[string]workspaceFileFactsEntry{}, limit: limit}
}

// lookup returns the entry for file when info still matches it.
func (c *workspaceFileFactsCache) lookup(file string, info os.FileInfo) (workspaceFileFactsEntry, bool) {
	if info == nil {
		return workspaceFileFactsEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[file]
	if !ok || entry.size != info.Size() || entry.modTime != info.ModTime().UnixNano() {
		return workspaceFileFactsEntry{}, false
	}
	return entry, true
}

// update applies change to file's entry, starting a fresh one when the file
// changed since it was recorded.
func (c *workspaceFileFactsCache) update(file string, info os.FileInfo, change func(*workspaceFileFactsEntry)) {
	if info == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[file]
	if !ok || entry.size != info.Size() || entry.modTime != info.ModTime().UnixNano() {
		if !ok && len(c.entries) >= c.limit {
			c.entries = map[string]workspaceFileFactsEntry{}
		}
		entry = workspaceFileFactsEntry{size: info.Size(), modTime: info.ModTime().UnixNano()}
	}
	change(&entry)
	c.entries[file] = entry
}

func (c *workspaceFileFactsCache) binary(file string, info os.FileInfo) (bool, bool) {
	entry, ok := c.lookup(file, info)
	return entry.binary, ok && entry.binaryKnown
}

func (c *workspaceFileFactsCache) setBinary(file string, info os.FileInfo, binary bool) {
	c.update(file, info, func(entry *workspaceFileFactsEntry) {
		entry.binaryKnown, entry.binary = true, binary
	})
}

func (c *workspaceFileFactsCache) lines(file string, info os.FileInfo) (int, bool, bool) {
	entry, ok := c.lookup(file, info)
	return entry.lines, entry.linesOK, ok && entry.linesKnown
}

func (c *workspaceFileFactsCache) setLines(file string, info os.FileInfo, lines int, ok bool) {
	c.update(file, info, func(entry *workspaceFileFactsEntry) {
		entry.linesKnown, entry.lines, entry.linesOK = true, lines, ok
	})
}
