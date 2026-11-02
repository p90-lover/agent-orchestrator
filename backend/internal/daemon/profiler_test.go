package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProfilerIsOffWithoutAnAddress(t *testing.T) {
	addr, err := startProfiler(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	if err != nil || addr != "" {
		t.Fatalf("startProfiler(\"\") = %q, %v; want off", addr, err)
	}
}

func TestProfilerRefusesANonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.5:0", "example.com:0", "nonsense"} {
		if got, err := startProfiler(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), addr); err == nil {
			t.Fatalf("startProfiler(%q) listened on %q; want refused", addr, got)
		}
	}
}

func TestProfilerServesPprofOnLoopbackUntilStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := startProfiler(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/debug/pprof/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), ".test") {
		t.Fatalf("cmdline = %d %q; want the test binary's command line", resp.StatusCode, body)
	}
	cancel()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 100; i++ {
		resp, err := client.Get("http://" + addr + "/debug/pprof/cmdline")
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the profiler kept serving after its context ended")
}
