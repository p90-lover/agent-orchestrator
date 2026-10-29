// Package idlepoll holds what the daemon's fast pollers (the CDC tail at 100ms,
// the preview poller at 250ms) share: an interval that stretches while polls
// find nothing, and error logging that reports a run of failures once instead
// of on every tick. Both pollers ran at full rate with nothing to do, and a
// poll that kept failing logged an error ten times a second, every line of it
// kept by the launcher.
package idlepoll

import (
	"log/slog"
	"time"
)

// Interval is a poll interval that doubles after each poll that found nothing
// (or failed), up to Max, and returns to Base as soon as one finds work.
type Interval struct {
	Base, Max time.Duration
	current   time.Duration
}

// NewInterval returns an Interval starting at base. A max below base means no
// backoff.
func NewInterval(base, max time.Duration) *Interval {
	if max < base {
		max = base
	}
	return &Interval{Base: base, Max: max, current: base}
}

// Next records whether the poll just run found work and returns the wait before
// the next one.
func (i *Interval) Next(active bool) time.Duration {
	if active {
		i.current = i.Base
		return i.current
	}
	i.current *= 2
	if i.current > i.Max {
		i.current = i.Max
	}
	return i.current
}

// RepeatEvery is how often a failure that keeps happening is logged again.
const RepeatEvery = time.Minute

// Failures logs a poller's errors: the first of a run at error level, the same
// run again at most once per RepeatEvery with its count, and the recovery once
// polls succeed again.
type Failures struct {
	logger  *slog.Logger
	msg     string
	clock   func() time.Time
	count   int
	lastLog time.Time
}

// NewFailures returns a Failures logging msg through logger. clock is injected
// in tests; nil means time.Now.
func NewFailures(logger *slog.Logger, msg string, clock func() time.Time) *Failures {
	if clock == nil {
		clock = time.Now
	}
	return &Failures{logger: logger, msg: msg, clock: clock}
}

// Record takes the result of one poll.
func (f *Failures) Record(err error) {
	now := f.clock()
	if err == nil {
		if f.count > 0 {
			f.logger.Info(f.msg+": recovered", "failed_polls", f.count)
			f.count = 0
		}
		return
	}
	f.count++
	if f.count == 1 || now.Sub(f.lastLog) >= RepeatEvery {
		f.logger.Error(f.msg, "err", err, "failed_polls", f.count)
		f.lastLog = now
	}
}
