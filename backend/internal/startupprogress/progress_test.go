package startupprogress

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log destination the ticker goroutine and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSpace(b.buf.String()), "\n")
}

func TestReporterNamesEachStageAndRepeatsItWithTheWorkDone(t *testing.T) {
	out := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(out, nil))
	var mu sync.Mutex
	work := Work{CPU: 1500 * time.Millisecond, IOBytes: 4096, HasIO: true}
	read := func() Work {
		mu.Lock()
		defer mu.Unlock()
		work.CPU += 250 * time.Millisecond
		work.IOBytes += 1 << 20
		return work
	}
	reporter := start(log, 10*time.Millisecond, time.Now, read)
	reporter.Stage("migrate", "migration", "9009_conversation_activity_page_keys_index.sql", "left", 2)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(out.lines()) >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	reporter.Stop()
	lines := out.lines()
	if len(lines) < 5 {
		t.Fatalf("want a line at each stage and one per interval, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], `msg="startup progress" stage=start elapsed_ms=`) {
		t.Fatalf("the first line names the start: %s", lines[0])
	}
	// A tick may land between Start and Stage; the stage line follows at once either way.
	first := 1
	for first < len(lines) && strings.Contains(lines[first], "stage=start ") {
		first++
	}
	if first > 2 || !strings.Contains(lines[first], `msg="startup progress" stage=migrate migration=9009_conversation_activity_page_keys_index.sql left=2 elapsed_ms=`) {
		t.Fatalf("a new stage is reported at once, with its detail:\n%s", strings.Join(lines, "\n"))
	}
	var cpu, io int64 = -1, -1
	for _, line := range lines {
		if !strings.Contains(line, " cpu_ms=") || !strings.Contains(line, " io_bytes=") {
			t.Fatalf("every line carries the work done: %s", line)
		}
		nextCPU, nextIO := field(t, line, "cpu_ms"), field(t, line, "io_bytes")
		if nextCPU <= cpu || nextIO <= io {
			t.Fatalf("the work is read again for every line:\n%s", strings.Join(lines, "\n"))
		}
		cpu, io = nextCPU, nextIO
	}
	for _, line := range lines[first:] {
		if !strings.Contains(line, "stage=migrate migration=9009_") {
			t.Fatalf("every later line repeats the stage: %s", line)
		}
	}

	stoppedAt := len(out.lines())
	time.Sleep(50 * time.Millisecond)
	reporter.Stage("listening")
	reporter.Stop()
	if got := len(out.lines()); got != stoppedAt {
		t.Fatalf("nothing is reported after Stop: %d lines, then %d", stoppedAt, got)
	}
}

func field(t *testing.T, line, key string) int64 {
	t.Helper()
	for _, part := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(part, key+"="); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				t.Fatalf("%s=%q: %v", key, value, err)
			}
			return n
		}
	}
	t.Fatalf("no %s in %s", key, line)
	return 0
}

func TestNilReporterReportsNothing(t *testing.T) {
	var reporter *Reporter
	reporter.Stage("migrate")
	reporter.Stop()
}

func TestSelfCountsThisProcesssWork(t *testing.T) {
	before := Self()
	// Some CPU, and some file I/O the counters must see.
	path := filepath.Join(t.TempDir(), "work")
	data := bytes.Repeat([]byte("x"), 4<<20)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	spin := time.Now()
	for sum := 0; time.Since(spin) < 100*time.Millisecond; sum++ {
		_ = sum * sum
	}
	after := Self()
	if after.CPU <= before.CPU {
		t.Fatalf("CPU time did not grow: %v then %v", before.CPU, after.CPU)
	}
	// Windows counts every read and write; Linux counts only what reached the
	// disk, which a cached file may not have yet.
	if runtime.GOOS == "windows" && after.IOBytes < before.IOBytes+(8<<20) {
		t.Fatalf("I/O did not count the 8 MiB written and read: %d then %d", before.IOBytes, after.IOBytes)
	}
}
