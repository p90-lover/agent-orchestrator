package versioncheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func fakeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent.exe")
	if err := os.WriteFile(bin, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestDidNotRunTellsAFailedCheckFromAVerdict(t *testing.T) {
	missing := exec.Command(filepath.Join(t.TempDir(), "missing-agent")).Run()
	for name, err := range map[string]error{
		"could not start":    fmt.Errorf("read Codex version: %w", missing),
		"ran out of time":    fmt.Errorf("read Qwen Code version: %w", context.DeadlineExceeded),
		"binary not on PATH": fmt.Errorf("read Cursor Agent version: %w", exec.ErrNotFound),
		"not allowed to run": fmt.Errorf("read Kimchi version: %w", os.ErrPermission),
	} {
		if !DidNotRun(err) {
			t.Errorf("%s: DidNotRun(%v) = false, want true", name, err)
		}
	}
	for name, err := range map[string]error{
		"too old":      errors.New("installed Qwen Code 0.1.0 is older than AO's tested minimum 0.2.0"),
		"unrecognized": errors.New(`unrecognized Cursor Agent version "dev" (AO requires 2026.01 or newer)`),
		"none":         nil,
	} {
		if DidNotRun(err) {
			t.Errorf("%s: DidNotRun(%v) = true, want false", name, err)
		}
	}
}

// 2026-11-12, machine out of memory: `codex --version` exited 1 inside its 5 s limit.
func TestOutputTriesACheckThatDidNotRunOnceMoreWithALongerLimit(t *testing.T) {
	forget()
	bin := fakeBinary(t)
	var limits []time.Duration
	probe := func(ctx context.Context, _ string) (string, error) {
		deadline, _ := ctx.Deadline()
		limits = append(limits, time.Until(deadline).Round(time.Second))
		if len(limits) == 1 {
			return "", fmt.Errorf("signal: killed: %w", context.DeadlineExceeded)
		}
		return "codex-cli 0.150.0", nil
	}
	output, err := Output(context.Background(), bin, probe)
	if err != nil || output != "codex-cli 0.150.0" {
		t.Fatalf("Output = %q, %v; want the second try's version", output, err)
	}
	if len(limits) != 2 || limits[1] <= limits[0] {
		t.Fatalf("limits = %v, want a second, longer try", limits)
	}

	tries := 0
	verdict := func(context.Context, string) (string, error) {
		tries++
		return "", errors.New("unrecognized Codex version")
	}
	if _, err := Output(context.Background(), bin, verdict); err == nil || tries != 1 {
		t.Fatalf("a judged version: err = %v after %d tries, want one try", err, tries)
	}
}

func TestAPassingCheckIsRememberedUntilTheBinaryChangesOrItExpires(t *testing.T) {
	forget()
	defer func() { now = time.Now }()
	clock := time.Unix(1_800_000_000, 0)
	now = func() time.Time { return clock }
	bin := fakeBinary(t)
	runs := 0
	probe := func(context.Context, string) error { runs++; return nil }

	for range 3 {
		if err := Check(context.Background(), bin, probe); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 {
		t.Fatalf("probe ran %d times, want once while its pass stands", runs)
	}

	// The agent was updated in place: another size.
	if err := os.WriteFile(bin, []byte("a newer binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), bin, probe); err != nil || runs != 2 {
		t.Fatalf("after the binary changed: err = %v, runs = %d; want a new check", err, runs)
	}

	clock = clock.Add(passTTL + time.Second)
	if err := Check(context.Background(), bin, probe); err != nil || runs != 3 {
		t.Fatalf("after the pass expired: err = %v, runs = %d; want a new check", err, runs)
	}

	// A failure is never remembered.
	failing := func(context.Context, string) error { runs++; return errors.New("too old") }
	forget()
	_ = Check(context.Background(), bin, failing)
	_ = Check(context.Background(), bin, failing)
	if runs != 5 {
		t.Fatalf("runs = %d, want every failing check to run", runs)
	}
}
