package preview

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The loop stretches its interval on polls that change nothing, so poll must
// tell a quiet scan from one that saw a preview file change.
func TestPollReportsWhetherAWatchedPreviewChanged(t *testing.T) {
	workspace := t.TempDir()
	entry := filepath.Join(workspace, "index.html")
	writeFile(t, entry, "<main>one</main>")
	svc := &fakePreviewSessions{sessions: []domain.SessionRecord{workerSession("ao-1", workspace, "index.html")}}
	poller := NewPoller(svc, svc, "http://127.0.0.1:3001", PollerConfig{Logger: discardLogger()})
	ctx := context.Background()

	if changed, err := poller.poll(ctx); err != nil || !changed {
		t.Fatalf("first scan: changed=%v err=%v, want a change (the entry is new to the poller)", changed, err)
	}
	if changed, _ := poller.poll(ctx); changed {
		t.Fatal("a scan with nothing touched reported a change")
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(entry, later, later); err != nil {
		t.Fatal(err)
	}
	if changed, _ := poller.poll(ctx); !changed {
		t.Fatal("a touched preview file was not reported as a change")
	}
	svc.sessions = nil
	if changed, _ := poller.poll(ctx); !changed {
		t.Fatal("a session going away was not reported as a change")
	}
}

type failingPreviewSource struct {
	mu    sync.Mutex
	polls int
}

func (f *failingPreviewSource) ListPreviewSessions(context.Context) ([]domain.SessionRecord, error) {
	f.mu.Lock()
	f.polls++
	f.mu.Unlock()
	return nil, errors.New("database is locked")
}

func (f *failingPreviewSource) SetPreview(context.Context, domain.SessionID, string) (domain.Session, error) {
	return domain.Session{}, nil
}

func (f *failingPreviewSource) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.polls }

func TestAFailingPreviewPollerLogsTheRunOnce(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	src := &failingPreviewSource{}
	ctx, cancel := context.WithCancel(context.Background())
	done := NewPoller(src, src, "http://127.0.0.1:3001", PollerConfig{Interval: time.Millisecond, MaxIdleInterval: -1, Logger: logger}).Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for src.count() < 30 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("%d failed polls logged %d errors, want 1:\n%s", src.count(), n, buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
