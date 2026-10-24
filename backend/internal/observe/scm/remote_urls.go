package scm

import (
	"context"
	"sync"
	"time"
)

// remoteURLTTL bounds how long a checkout's remote URLs are reused. Listing
// them runs 1 + 2 per remote git processes, and every 30 s poll listed them
// again for every project; remotes change far less often than that.
const remoteURLTTL = 2 * time.Minute

type remoteURLCache struct {
	mu      sync.Mutex
	entries map[string]remoteURLEntry
}

type remoteURLEntry struct {
	urls []string
	at   time.Time
}

// get returns path's remote URLs, running load at most once per remoteURLTTL.
// A lookup cut short by ctx is returned but not remembered.
func (c *remoteURLCache) get(ctx context.Context, path string, now time.Time, load func(context.Context, string) []string) []string {
	c.mu.Lock()
	entry, ok := c.entries[path]
	c.mu.Unlock()
	if ok && !now.Before(entry.at) && now.Sub(entry.at) < remoteURLTTL {
		return entry.urls
	}
	urls := load(ctx, path)
	if ctx.Err() != nil {
		return urls
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]remoteURLEntry)
	}
	for key, old := range c.entries {
		if now.Sub(old.at) >= remoteURLTTL || now.Before(old.at) {
			delete(c.entries, key)
		}
	}
	c.entries[path] = remoteURLEntry{urls: urls, at: now}
	return urls
}

// originURL is resolveGitOriginURL behind originURLs. A project registered
// without an origin (a folder with no remote) asked git again on every poll,
// because an empty answer is never stored on the project.
func (o *Observer) originURL(ctx context.Context, path string) string {
	urls := o.originURLs.get(ctx, path, time.Now(), func(_ context.Context, path string) []string {
		if url := resolveGitOriginURLFunc(path); url != "" {
			return []string{url}
		}
		return nil
	})
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}
