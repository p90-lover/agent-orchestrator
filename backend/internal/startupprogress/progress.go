// Package startupprogress writes what a starting daemon is doing, before it
// listens, to the daemon's log, so the process that started it can tell a slow
// start from a hung one.
//
// On 2026-11-16 the first start after an update built migration 9009's index
// over a 4.7 GB ao.db on a busy hard disk. The daemon printed nothing for 15
// minutes, so the Coding Tools launcher, which ends a daemon that is not ready
// by then, could not tell the build from a hang and killed it. The build ran in
// one transaction, so it rolled back, and the next start began it again.
//
// A Reporter writes one Message line when a stage begins and one every interval
// while it lasts. Each line carries the process's CPU time and I/O so far, which
// a stuck daemon does not add to and a working one does.
package startupprogress

import (
	"log/slog"
	"sync"
	"time"
)

// Message is the msg of every progress line. Supervisors match it in the
// daemon's output; keep it and the field names stable.
const Message = "startup progress"

// DefaultInterval is how often a lasting stage is reported again.
const DefaultInterval = 15 * time.Second

// Work is what the process has done since it started.
type Work struct {
	// CPU is the process's user and kernel time.
	CPU time.Duration
	// IOBytes counts the bytes the process read and wrote, when HasIO.
	IOBytes uint64
	HasIO   bool
}

// Reporter reports the stages of a daemon's start. Its methods are safe on a
// nil Reporter, which reports nothing.
type Reporter struct {
	log     *slog.Logger
	every   time.Duration
	started time.Time
	now     func() time.Time
	work    func() Work

	mu    sync.Mutex
	stage string
	attrs []any
	done  bool

	stop    chan struct{}
	stopped chan struct{}
}

// Start reports the stage "start" now and every interval until Stop.
func Start(log *slog.Logger, every time.Duration) *Reporter {
	return start(log, every, time.Now, Self)
}

func start(log *slog.Logger, every time.Duration, now func() time.Time, work func() Work) *Reporter {
	if every <= 0 {
		every = DefaultInterval
	}
	r := &Reporter{log: log, every: every, started: now(), now: now, work: work,
		stage: "start", stop: make(chan struct{}), stopped: make(chan struct{})}
	r.report()
	go r.loop()
	return r
}

// Stage reports that the start has moved on to name, with attrs (slog
// key-value pairs) saying more. Later lines repeat both until the next Stage.
func (r *Reporter) Stage(name string, attrs ...any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.stage, r.attrs = name, attrs
	r.mu.Unlock()
	r.report()
}

// Stop ends the reports: the daemon is listening, or its start failed. It
// waits for a report under way, so no line follows it.
func (r *Reporter) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		<-r.stopped
		return
	}
	r.done = true
	r.mu.Unlock()
	close(r.stop)
	<-r.stopped
}

func (r *Reporter) loop() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.every)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.report()
		}
	}
}

func (r *Reporter) report() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done || r.log == nil {
		return
	}
	work := r.work()
	fields := make([]any, 0, len(r.attrs)+8)
	fields = append(fields, "stage", r.stage)
	fields = append(fields, r.attrs...)
	fields = append(fields, "elapsed_ms", r.now().Sub(r.started).Milliseconds(), "cpu_ms", work.CPU.Milliseconds())
	if work.HasIO {
		fields = append(fields, "io_bytes", work.IOBytes)
	}
	r.log.Info(Message, fields...)
}
