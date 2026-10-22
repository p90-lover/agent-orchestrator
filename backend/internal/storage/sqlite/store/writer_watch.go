package store

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// writerReportGap keeps a long writer from flooding the log: one report a minute.
const writerReportGap = time.Minute

// WatchWriter reports a write that holds the store's single writer for longer
// than holdWarn while other writes wait for it. Every write takes that one lock,
// a chat launch's CreateSession among them, so one long writer stalled every new
// chat with nothing in the log (seen 2026-10-22: async session creates that
// usually answer in 140 ms took 57 s to over 5 min while reads stayed fast). The
// report names the store methods on the holder's stack. It checks once a second
// and costs nothing while writes are short; it returns when ctx ends.
func (s *Store) WatchWriter(ctx context.Context, logger *slog.Logger, holdWarn time.Duration) {
	if logger == nil || holdWarn <= 0 {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastReport time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			held := s.writeMu.heldFor(now)
			waiting := s.writeMu.waiting.Load()
			if held < holdWarn || waiting == 0 || now.Sub(lastReport) < writerReportGap {
				continue
			}
			lastReport = now
			pool := s.writeDB.Stats()
			logger.Warn("sqlite writer held while writes wait",
				"held", held.Round(time.Millisecond), "waiting", waiting,
				"poolWaitCount", pool.WaitCount, "poolWait", pool.WaitDuration.Round(time.Millisecond),
				"holder", writerHolders(allStacks()))
		}
	}
}

// allStacks is every goroutine's stack; it pauses the process briefly, so it
// runs only when a report is due.
func allStacks() []byte {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) || len(buf) >= 16<<20 {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

var storeMethod = regexp.MustCompile(`storage/sqlite/store\.\(\*Store\)\.([A-Za-z0-9_]+)`)

// writerHolders names the store methods running on goroutines that are inside
// the store but not queued on its writer lock: the holder, and any reader. Ones
// in a transaction or an Exec (writing) come first. Each goroutine is one
// "a<-b<-c" chain, innermost first; at most five, deduplicated.
func writerHolders(stacks []byte) string {
	var writing, other [][]byte
	for _, goroutine := range bytes.Split(stacks, []byte("\n\n")) {
		if bytes.Contains(goroutine, []byte("(*contextMutex).Lock")) {
			continue
		}
		if bytes.Contains(goroutine, []byte("database/sql.(*Tx)")) || bytes.Contains(goroutine, []byte("ExecContext")) {
			writing = append(writing, goroutine)
		} else {
			other = append(other, goroutine)
		}
	}
	var chains []string
	seen := map[string]bool{}
	for _, goroutine := range append(writing, other...) {
		var names []string
		for _, match := range storeMethod.FindAllSubmatch(goroutine, -1) {
			name := string(match[1])
			if len(names) == 0 || names[len(names)-1] != name {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		chain := strings.Join(names, "<-")
		if seen[chain] {
			continue
		}
		seen[chain] = true
		if chains = append(chains, chain); len(chains) == 5 {
			break
		}
	}
	if len(chains) == 0 {
		return "unknown (not inside a store method right now)"
	}
	return strings.Join(chains, " | ")
}
