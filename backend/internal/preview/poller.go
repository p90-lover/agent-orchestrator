package preview

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/idlepoll"
)

// DefaultPollInterval is the preview poller's scan interval when none is configured.
const DefaultPollInterval = 250 * time.Millisecond

// DefaultMaxIdleInterval caps how far the interval stretches while no watched
// preview changes: each scan stats every selected preview file on disk, four
// times a second with nothing changing. A change after a quiet spell shows
// within this.
const DefaultMaxIdleInterval = time.Second

// sessionPreviewSource lists live sessions. Records need only ID, Kind,
// IsTerminated and the workspace/preview metadata the poller compares.
type sessionPreviewSource interface {
	ListPreviewSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

type previewSetter interface {
	SetPreview(ctx context.Context, id domain.SessionID, previewURL string) (domain.Session, error)
}

// PollerConfig configures preview poller timing and logging. A negative
// MaxIdleInterval means no idle backoff.
type PollerConfig struct {
	Interval        time.Duration
	MaxIdleInterval time.Duration
	Logger          *slog.Logger
}

// Poller watches explicitly selected workspace previews and persists refreshes
// through the normal session service path. It never chooses a preview for a
// fresh worker: selection belongs to `ao preview`, a managed server start, or
// deliberate user navigation.
type Poller struct {
	source      sessionPreviewSource
	setter      previewSetter
	baseURL     string
	interval    time.Duration
	maxInterval time.Duration
	logger      *slog.Logger
	seen        map[domain.SessionID]entryState
}

type entryState struct {
	path    string
	modUnix int64
	size    int64
	// cleared is set when the poller itself cleared the preview URL because the
	// workspace entry was missing. When the file reappears, shouldRefresh uses
	// this to re-discover even though the revision was bumped by the clear.
	cleared bool
}

// NewPoller constructs a preview poller over the supplied session source and setter.
func NewPoller(source sessionPreviewSource, setter previewSetter, baseURL string, cfg PollerConfig) *Poller {
	p := &Poller{
		source:      source,
		setter:      setter,
		baseURL:     baseURL,
		interval:    cfg.Interval,
		maxInterval: cfg.MaxIdleInterval,
		logger:      cfg.Logger,
		seen:        map[domain.SessionID]entryState{},
	}
	if p.interval <= 0 {
		p.interval = DefaultPollInterval
	}
	if p.maxInterval == 0 {
		p.maxInterval = max(DefaultMaxIdleInterval, p.interval)
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	return p
}

// Start runs an immediate poll followed by interval polling until ctx is
// cancelled. The returned channel closes after the goroutine exits. While no
// watched preview changes the wait between polls doubles up to the configured
// maximum; a change brings it back to the interval.
func (p *Poller) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := idlepoll.NewInterval(p.interval, p.maxInterval)
		failures := idlepoll.NewFailures(p.logger, "preview poller: poll failed", nil)
		changed, err := p.poll(ctx)
		failures.Record(err)
		timer := time.NewTimer(wait.Next(changed))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				changed, err = p.poll(ctx)
				failures.Record(err)
				timer.Reset(wait.Next(changed))
			}
		}
	}()
	return done
}

// Poll performs one deterministic scan of active worker sessions.
func (p *Poller) Poll(ctx context.Context) error {
	_, err := p.poll(ctx)
	return err
}

// poll scans once and reports whether any watched entry changed (a preview set
// or cleared, a file touched, a session appearing or going).
func (p *Poller) poll(ctx context.Context) (bool, error) {
	before := maps.Clone(p.seen)
	err := p.scan(ctx)
	return !maps.Equal(before, p.seen), err
}

func (p *Poller) scan(ctx context.Context) error {
	if p.source == nil || p.setter == nil {
		return nil
	}
	sessions, err := p.source.ListPreviewSessions(ctx)
	if err != nil {
		return fmt.Errorf("preview poller list sessions: %w", err)
	}
	activeIDs := make(map[domain.SessionID]struct{}, len(sessions))
	for _, sess := range sessions {
		if sess.IsTerminated {
			continue
		}
		activeIDs[sess.ID] = struct{}{}
		if sess.Kind != domain.KindWorker {
			continue
		}
		storedEntry, workspaceOwned := StoredWorkspaceEntry(sess.Metadata.PreviewURL, sess.ID)
		previous, seenBefore := p.seen[sess.ID]
		restoringCleared := false
		if !workspaceOwned {
			// Only restore an entry after the poller itself cleared it because
			// the selected file temporarily disappeared. A blank fresh session
			// or an explicit user clear must remain blank.
			if !seenBefore || !previous.cleared || previous.path == "" {
				continue
			}
			storedEntry = previous.path
			workspaceOwned = true
			restoringCleared = true
		}
		entry, ok := EntryAtPath(sess.Metadata.WorkspacePath, storedEntry)
		if !ok {
			if workspaceOwned {
				if !restoringCleared {
					if _, err := p.setter.SetPreview(ctx, sess.ID, ""); err != nil {
						p.logger.Error("preview poller: failed to clear stale preview",
							"session", sess.ID, "err", err)
					}
				}
				p.seen[sess.ID] = entryState{path: storedEntry, cleared: true}
			}
			continue
		}
		state := stateFor(entry)
		if seenBefore && previous == state {
			continue
		}
		target, err := FileURL(p.baseURL, sess.ID, entry.Path)
		if err != nil {
			p.logger.Error("preview poller: cannot build isolated preview URL", "session", sess.ID, "err", err)
			p.seen[sess.ID] = state
			continue
		}
		if !p.shouldRefresh(sess, target, seenBefore, workspaceOwned) {
			p.seen[sess.ID] = state
			continue
		}
		if _, err := p.setter.SetPreview(ctx, sess.ID, target); err != nil {
			return fmt.Errorf("preview poller set preview %s: %w", sess.ID, err)
		}
		p.seen[sess.ID] = state
	}
	for id := range p.seen {
		if _, ok := activeIDs[id]; !ok {
			delete(p.seen, id)
		}
	}
	return nil
}

func (p *Poller) shouldRefresh(sess domain.SessionRecord, target string, seenBefore, workspaceOwned bool) bool {
	current := strings.TrimSpace(sess.Metadata.PreviewURL)
	if current == "" {
		if !seenBefore {
			return sess.Metadata.PreviewRevision == 0
		}
		previous := p.seen[sess.ID]
		return previous.cleared
	}
	if current == target {
		return seenBefore
	}
	return workspaceOwned
}

func stateFor(entry Entry) entryState {
	return entryState{path: entry.Path, modUnix: entry.ModTime.UnixNano(), size: entry.Size}
}
