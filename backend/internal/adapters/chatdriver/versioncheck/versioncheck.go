// Package versioncheck runs a chat driver's `<agent> --version` probe so that a check which could
// not run is told apart from a version that is really too old.
//
// Every driver called any failure of its probe "incompatible", which AO and the app treat as final.
// With the machine out of memory (2026-11-12) `codex --version` exited 1 within its 5 s limit, and a
// chat whose Codex was fine failed for good with "read Codex version: exit status 1
// (CHAT_DRIVER_INCOMPATIBLE)". A probe that could not run is now tried once more with a longer limit
// and, failing again, reported as unavailable (a passing failure). A version that was read keeps its
// verdict. A passing check is remembered for a while per binary, so a launch starts one process fewer.
package versioncheck

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Limits for the two tries: the usual one, then a longer one for a busy machine.
var (
	firstTimeout  = 5 * time.Second
	secondTimeout = 15 * time.Second
	// passTTL bounds how long a passing check stands in for a new one. Only passes are kept: a
	// failure is always checked again, and a binary replaced since (another size or time) is too.
	passTTL = 10 * time.Minute
	now     = time.Now
)

type passKey struct {
	bin     string
	size    int64
	modTime time.Time
}

type pass struct {
	output string
	at     time.Time
}

var (
	passesMu sync.Mutex
	passes   = map[passKey]pass{}
)

// Output runs probe for bin and returns its output (a version for the caller to judge). A cached
// pass for the same binary returns its output without running anything. An error from Output is
// the probe's: DidNotRun tells whether it is a check that could not run.
func Output(ctx context.Context, bin string, probe func(context.Context, string) (string, error)) (string, error) {
	key, keyed := keyFor(bin)
	if keyed {
		passesMu.Lock()
		cached, ok := passes[key]
		passesMu.Unlock()
		if ok && now().Sub(cached.at) < passTTL {
			return cached.output, nil
		}
	}
	output, err := run(ctx, bin, probe, firstTimeout)
	if err != nil && DidNotRun(err) && ctx.Err() == nil {
		output, err = run(ctx, bin, probe, secondTimeout)
	}
	return output, err
}

// Remember records that bin's output passed the caller's own version check.
func Remember(bin, output string) {
	key, keyed := keyFor(bin)
	if !keyed {
		return
	}
	passesMu.Lock()
	defer passesMu.Unlock()
	if len(passes) > 64 {
		passes = map[passKey]pass{}
	}
	passes[key] = pass{output: output, at: now()}
}

// Check is Output for a probe that judges the version itself (it returns an error for a version too
// old). Its passes are remembered.
func Check(ctx context.Context, bin string, probe func(context.Context, string) error) error {
	_, err := Output(ctx, bin, func(ctx context.Context, bin string) (string, error) {
		return "", probe(ctx, bin)
	})
	if err == nil {
		Remember(bin, "")
	}
	return err
}

// DidNotRun reports whether err says the version check itself failed to run or finish: the process
// could not start, exited with a failure, or ran out of time. A version that was read and judged
// (too old, unrecognized) is not one.
func DidNotRun(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	var pathErr *fs.PathError
	return errors.As(err, &exitErr) || errors.As(err, &pathErr) ||
		errors.Is(err, exec.ErrNotFound) || errors.Is(err, exec.ErrWaitDelay) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrPermission)
}

func run(ctx context.Context, bin string, probe func(context.Context, string) (string, error), limit time.Duration) (string, error) {
	tryCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return probe(tryCtx, bin)
}

func keyFor(bin string) (passKey, bool) {
	info, err := os.Stat(bin)
	if err != nil {
		return passKey{}, false
	}
	return passKey{bin: bin, size: info.Size(), modTime: info.ModTime()}, true
}

// forget drops every remembered pass (tests).
func forget() {
	passesMu.Lock()
	passes = map[passKey]pass{}
	passesMu.Unlock()
}
