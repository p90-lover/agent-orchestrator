package idlepoll

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestIntervalDoublesWhileIdleAndResetsOnWork(t *testing.T) {
	i := NewInterval(100*time.Millisecond, 400*time.Millisecond)
	var got []time.Duration
	for _, active := range []bool{false, false, false, false, true, false} {
		got = append(got, i.Next(active))
	}
	want := []time.Duration{200, 400, 400, 400, 100, 200}
	for n := range want {
		if got[n] != want[n]*time.Millisecond {
			t.Fatalf("intervals = %v, want %v ms", got, want)
		}
	}
	if fixed := NewInterval(time.Second, -1); fixed.Next(false) != time.Second {
		t.Fatal("a max below base means no backoff")
	}
}

func TestFailuresLogARunOnceAMinuteAndItsRecovery(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(1000, 0)
	f := NewFailures(slog.New(slog.NewTextHandler(&buf, nil)), "cdc poller: poll failed", func() time.Time { return now })
	boom := errors.New("database is locked")
	for range 100 { // ten seconds of a 100ms poll failing
		f.Record(boom)
		now = now.Add(100 * time.Millisecond)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("logged %d errors for one run, want 1:\n%s", n, buf.String())
	}
	now = now.Add(RepeatEvery)
	f.Record(boom)
	if n := strings.Count(buf.String(), "level=ERROR"); n != 2 || !strings.Contains(buf.String(), "failed_polls=101") {
		t.Fatalf("want the run logged again after a minute with its count:\n%s", buf.String())
	}
	f.Record(nil)
	f.Record(nil)
	if n := strings.Count(buf.String(), "recovered"); n != 1 {
		t.Fatalf("want one recovery line:\n%s", buf.String())
	}
	f.Record(boom)
	if n := strings.Count(buf.String(), "level=ERROR"); n != 3 {
		t.Fatalf("a new run after recovery is logged at once:\n%s", buf.String())
	}
}
