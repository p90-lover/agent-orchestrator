// Package livesessions shares one listing of the live (not terminated) sessions
// between the periodic observers: the runtime reaper (every 5s), the activity
// and SCM observers (every 30s) and the auto-review sweep (every minute). Each
// used to list every session ever created on its own. Any write resets SQLite's
// reader page cache, so each listing read the whole sessions table and its
// prompt overflow pages from disk again, about 18 times a minute while idle.
package livesessions

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// DefaultMaxAge is how old a shared listing may be when it is handed out. It is
// shorter than the reaper's 5s tick, so the reaper reads afresh each time and
// the slower observers mostly reuse what the reaper just read.
const DefaultMaxAge = 4 * time.Second

// Source reads the live sessions from storage.
type Source interface {
	ListLiveSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// Config holds optional knobs; zero values fall back to defaults.
type Config struct {
	// MaxAge bounds how old a listing may be when it is reused. <=0 means
	// DefaultMaxAge.
	MaxAge time.Duration
	// Clock is injected in tests. nil means time.Now.
	Clock func() time.Time
}

// Snapshot hands out one listing to every caller within MaxAge of the read.
// Callers that arrive while a read is running wait for it rather than start
// their own. A failed read is not kept.
type Snapshot struct {
	src    Source
	maxAge time.Duration
	clock  func() time.Time

	mu       sync.Mutex
	records  []domain.SessionRecord
	readAt   time.Time
	valid    bool
	gen      uint64 // bumped by Invalidate: a read begun before it is neither kept nor joined
	inflight *read
}

type read struct {
	gen     uint64
	done    chan struct{}
	records []domain.SessionRecord
	err     error
}

// New returns a Snapshot over src.
func New(src Source, cfg Config) *Snapshot {
	s := &Snapshot{src: src, maxAge: cfg.MaxAge, clock: cfg.Clock}
	if s.maxAge <= 0 {
		s.maxAge = DefaultMaxAge
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s
}

// ListLiveSessions returns the live sessions, read at most MaxAge ago. The
// slice is the caller's own; the records in it are shared and must be treated
// as read-only.
func (s *Snapshot) ListLiveSessions(ctx context.Context) ([]domain.SessionRecord, error) {
	s.mu.Lock()
	if s.valid && s.clock().Sub(s.readAt) < s.maxAge {
		out := slices.Clone(s.records)
		s.mu.Unlock()
		return out, nil
	}
	r := s.inflight
	if r == nil || r.gen != s.gen {
		r = &read{gen: s.gen, done: make(chan struct{})}
		s.inflight = r
		started := s.clock()
		s.mu.Unlock()
		// The read runs detached from any one caller's cancellation, so a caller
		// that gives up does not fail the others waiting on the same read.
		r.records, r.err = s.src.ListLiveSessions(context.WithoutCancel(ctx))
		s.mu.Lock()
		if r.err == nil && r.gen == s.gen {
			s.records, s.readAt, s.valid = r.records, started, true
		}
		if s.inflight == r {
			s.inflight = nil
		}
		close(r.done)
		s.mu.Unlock()
	} else {
		s.mu.Unlock()
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return slices.Clone(r.records), nil
}

// Invalidate drops the shared listing, so the next call reads afresh. The
// daemon calls it before a pass that must see every write made so far (the
// boot-time runtime reconcile).
func (s *Snapshot) Invalidate() {
	s.mu.Lock()
	s.valid = false
	s.gen++
	s.mu.Unlock()
}
