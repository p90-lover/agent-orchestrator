package cdc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type scriptedSource struct {
	mu     sync.Mutex
	polls  int
	events []Event
	err    error
}

func (s *scriptedSource) EventsAfter(_ context.Context, after int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	if s.err != nil {
		return nil, s.err
	}
	var out []Event
	for _, e := range s.events {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *scriptedSource) LatestSeq(context.Context) (int64, error) { return 0, nil }

func (s *scriptedSource) pollCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.polls }

func TestAnIdlePollerSlowsDownAndStillDeliversPromptly(t *testing.T) {
	src := &scriptedSource{}
	bcast := NewBroadcaster()
	delivered := make(chan Event, 4)
	bcast.Subscribe(func(e Event) { delivered <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPoller(src, bcast, PollerConfig{Interval: 5 * time.Millisecond, MaxIdleInterval: 40 * time.Millisecond})
	done := p.Start(ctx)

	time.Sleep(400 * time.Millisecond)
	// At a fixed 5ms that is ~80 polls; doubling to 40ms makes it about a dozen.
	if n := src.pollCount(); n > 25 {
		t.Fatalf("idle polls in 400ms = %d, want the interval to stretch", n)
	}
	src.mu.Lock()
	src.events = append(src.events, Event{Seq: 1})
	src.mu.Unlock()
	select {
	case e := <-delivered:
		if e.Seq != 1 {
			t.Fatalf("delivered %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("an event after a quiet spell was not delivered within the idle cap")
	}
	cancel()
	<-done
}

func TestAFailingPollerLogsTheRunOnce(t *testing.T) {
	var buf syncBuffer
	src := &scriptedSource{err: errors.New("database is locked")}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewPoller(src, NewBroadcaster(), PollerConfig{Interval: time.Millisecond, MaxIdleInterval: -1, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	done := p.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for src.pollCount() < 50 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("%d failed polls logged %d errors, want 1:\n%s", src.pollCount(), n, buf.String())
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
