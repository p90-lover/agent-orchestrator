package project

import (
	"context"
	"sync"
	"time"
)

// defaultBranchTTL bounds how long project read models reuse a resolved default
// branch. Resolving runs several git processes per project, and List, Get and
// every project response resolved it again on each request: with one chat open
// the daemon spent about 9 s of each minute waiting on those processes. The
// value is advisory; workspace spawns still resolve live through gitdefault.
const defaultBranchTTL = 30 * time.Second

// maxDefaultBranchEntries caps the memo; expired entries are dropped first.
const maxDefaultBranchEntries = 256

type defaultBranchCache struct {
	mu      sync.Mutex
	entries map[string]defaultBranchEntry
}

type defaultBranchEntry struct {
	branch string
	at     time.Time
}

// resolve returns path's default branch, running resolve at most once per
// defaultBranchTTL. A lookup cut short by ctx is returned but not remembered.
func (c *defaultBranchCache) resolve(ctx context.Context, path string, now time.Time, resolve func(context.Context, string) string) string {
	c.mu.Lock()
	entry, ok := c.entries[path]
	c.mu.Unlock()
	if ok && !now.Before(entry.at) && now.Sub(entry.at) < defaultBranchTTL {
		return entry.branch
	}
	branch := resolve(ctx, path)
	if ctx.Err() != nil {
		return branch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]defaultBranchEntry)
	}
	if len(c.entries) >= maxDefaultBranchEntries {
		for key, old := range c.entries {
			if now.Sub(old.at) >= defaultBranchTTL || now.Before(old.at) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= maxDefaultBranchEntries {
			clear(c.entries)
		}
	}
	c.entries[path] = defaultBranchEntry{branch: branch, at: now}
	return branch
}

// forget drops path so the next read resolves again, after AO itself changed
// the repository's registration.
func (c *defaultBranchCache) forget(path string) {
	c.mu.Lock()
	delete(c.entries, path)
	c.mu.Unlock()
}
