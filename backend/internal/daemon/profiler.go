package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

// profilerAddrEnv opts the daemon into serving Go's profiler, for finding what
// uses its CPU on a user's machine (`go tool pprof http://<addr>/debug/pprof/profile`).
// Off unless set, and only on a loopback address: a profile shows internals.
const profilerAddrEnv = "AO_PPROF_ADDR"

// startProfiler serves net/http/pprof on addr until ctx ends. It is a separate
// listener with its own mux, so the API server never exposes these routes.
// It returns the address it listens on ("" when addr is empty).
func startProfiler(ctx context.Context, log *slog.Logger, addr string) (string, error) {
	if addr == "" {
		return "", nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", profilerAddrEnv, addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("%s=%q: the profiler only listens on a loopback address", profilerAddrEnv, addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", profilerAddrEnv, addr, err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("profiler stopped", "err", err)
		}
	}()
	return listener.Addr().String(), nil
}

// startProfilerFromEnv starts the profiler when AO_PPROF_ADDR is set. A bad
// value is logged, never fatal: profiling must not stop the daemon starting.
func startProfilerFromEnv(ctx context.Context, log *slog.Logger) {
	addr, err := startProfiler(ctx, log, os.Getenv(profilerAddrEnv))
	if err != nil {
		log.Warn("profiler disabled", "err", err)
		return
	}
	if addr != "" {
		log.Info("profiler listening", "addr", addr)
	}
}
