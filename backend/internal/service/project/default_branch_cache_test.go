package project

import (
	"context"
	"testing"
	"time"
)

func TestDefaultBranchCacheResolvesOncePerTTL(t *testing.T) {
	var cache defaultBranchCache
	calls := 0
	resolve := func(context.Context, string) string {
		calls++
		return "main"
	}
	ctx := context.Background()
	start := time.Unix(1_700_000_000, 0)

	for _, at := range []time.Duration{0, time.Second, defaultBranchTTL - time.Nanosecond} {
		if got := cache.resolve(ctx, "/repo", start.Add(at), resolve); got != "main" {
			t.Fatalf("branch = %q, want main", got)
		}
	}
	if calls != 1 {
		t.Fatalf("resolved %d times within the TTL, want 1", calls)
	}
	cache.resolve(ctx, "/repo", start.Add(defaultBranchTTL), resolve)
	if calls != 2 {
		t.Fatalf("an expired entry was reused (%d calls)", calls)
	}
	cache.resolve(ctx, "/other", start.Add(defaultBranchTTL), resolve)
	if calls != 3 {
		t.Fatalf("paths share an entry (%d calls)", calls)
	}
	cache.forget("/repo")
	cache.resolve(ctx, "/repo", start.Add(defaultBranchTTL), resolve)
	if calls != 4 {
		t.Fatalf("a forgotten path was reused (%d calls)", calls)
	}
	// A clock that moved backwards never extends an entry's life.
	cache.resolve(ctx, "/repo", start, resolve)
	if calls != 5 {
		t.Fatalf("an entry from the future was reused (%d calls)", calls)
	}
}

func TestDefaultBranchCacheDoesNotRememberCancelledLookups(t *testing.T) {
	var cache defaultBranchCache
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now()
	cache.resolve(ctx, "/repo", now, func(context.Context, string) string { return "" })
	got := cache.resolve(context.Background(), "/repo", now, func(context.Context, string) string { return "main" })
	if got != "main" {
		t.Fatalf("branch = %q after a cancelled lookup, want a fresh main", got)
	}
}

func TestDefaultBranchCacheStaysBounded(t *testing.T) {
	var cache defaultBranchCache
	now := time.Now()
	for i := range maxDefaultBranchEntries * 3 {
		cache.resolve(context.Background(), string(rune('a'+i%26))+time.Duration(i).String(), now,
			func(context.Context, string) string { return "main" })
	}
	if len(cache.entries) > maxDefaultBranchEntries {
		t.Fatalf("cache holds %d entries, cap %d", len(cache.entries), maxDefaultBranchEntries)
	}
}
