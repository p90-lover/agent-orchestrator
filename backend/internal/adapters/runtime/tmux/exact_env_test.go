package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A tmux pane starts from the server's environment, so an exact environment
// is refused, never quietly merged into it.
func TestCreateRefusesAnExactEnvironment(t *testing.T) {
	r, fr := newTestRuntime(0)
	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "sess-1",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"ssh"},
		Env:           map[string]string{"SSH_ASKPASS_REQUIRE": "never"},
		EnvExact:      true,
	})
	if err == nil || !strings.Contains(err.Error(), "exact environment") {
		t.Fatalf("Create err = %v, want the exact environment refused", err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("tmux ran %d commands for a refused create", len(fr.calls))
	}
}
