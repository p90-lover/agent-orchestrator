package scm

import (
	"context"
	"testing"
	"time"
)

func TestRemoteURLCacheListsOncePerTTL(t *testing.T) {
	var cache remoteURLCache
	calls := 0
	load := func(context.Context, string) []string {
		calls++
		return []string{"https://github.com/o/r.git"}
	}
	ctx := context.Background()
	start := time.Unix(1_700_000_000, 0)
	for _, at := range []time.Duration{0, 30 * time.Second, remoteURLTTL - time.Nanosecond} {
		if got := cache.get(ctx, "/repo", start.Add(at), load); len(got) != 1 {
			t.Fatalf("urls = %v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("listed %d times within the TTL, want 1", calls)
	}
	cache.get(ctx, "/repo", start.Add(remoteURLTTL), load)
	if calls != 2 {
		t.Fatalf("an expired entry was reused (%d calls)", calls)
	}
	if len(cache.entries) != 1 {
		t.Fatalf("expired entries kept: %d", len(cache.entries))
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cache.get(cancelled, "/other", start, func(context.Context, string) []string { return nil })
	if _, ok := cache.entries["/other"]; ok {
		t.Fatal("a cancelled listing was remembered")
	}
}

func TestOriginURLRemembersAProjectWithoutOne(t *testing.T) {
	old := resolveGitOriginURLFunc
	defer func() { resolveGitOriginURLFunc = old }()
	lookups := 0
	resolveGitOriginURLFunc = func(path string) string {
		lookups++
		if path == "/with-origin" {
			return "https://github.com/o/r.git"
		}
		return ""
	}
	o := &Observer{}
	for range 3 {
		if got := o.originURL(context.Background(), "/no-remote"); got != "" {
			t.Fatalf("origin = %q, want none", got)
		}
	}
	if got := o.originURL(context.Background(), "/with-origin"); got != "https://github.com/o/r.git" {
		t.Fatalf("origin = %q", got)
	}
	if lookups != 2 {
		t.Fatalf("git asked %d times for 2 projects across 4 polls, want 2", lookups)
	}
}
