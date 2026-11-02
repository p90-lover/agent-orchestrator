package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workspaceGateFree reports whether nobody holds the project's workspace gate.
func workspaceGateFree(m *Manager, project domain.ProjectID) bool {
	m.workspaceGateMu.Lock()
	mu := m.workspaceGates[project]
	m.workspaceGateMu.Unlock()
	if mu == nil {
		return true
	}
	if !mu.TryLock() {
		return false
	}
	mu.Unlock()
	return true
}

// The workspace gate serializes worktree work, not the agent handshake: a
// second chat in the same project must not wait for the first one's agent.
func TestChatSpawnReleasesWorkspaceGateBeforeAgentHandshake(t *testing.T) {
	for _, async := range []bool{false, true} {
		launcher := &recordingLauncher{}
		m, _, _ := newChatManager(launcher)
		deferred := deferredBackground(m)
		var freeDuringStart bool
		launcher.beforeStart = func(ChatStart) { freeDuringStart = workspaceGateFree(m, chatTestProject) }

		cfg := asyncChatSpawnConfig("do the thing")
		cfg.Async = async
		if _, _, _, err := m.Spawn(context.Background(), cfg); err != nil {
			t.Fatalf("async=%v spawn: %v", async, err)
		}
		for _, work := range *deferred {
			work()
		}
		if len(launcher.started) != 1 {
			t.Fatalf("async=%v started %d controllers, want 1", async, len(launcher.started))
		}
		if !freeDuringStart {
			t.Fatalf("async=%v: the workspace gate was held through the agent handshake", async)
		}
	}
}

// gateProbeWorkspace reports, at each teardown, whether the gate was held.
type gateProbeWorkspace struct {
	*fakeWorkspace
	onDestroy func()
}

func (w *gateProbeWorkspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	w.onDestroy()
	return w.fakeWorkspace.Destroy(ctx, info)
}

// A launch that fails after the gate was released still rolls its worktree
// back, under the gate again, and leaves the gate free.
func TestChatSpawnRollbackRetakesWorkspaceGate(t *testing.T) {
	for _, async := range []bool{false, true} {
		launcher := &recordingLauncher{startErr: errors.New("handshake failed")}
		st := newFakeStore()
		st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
		ws := &gateProbeWorkspace{fakeWorkspace: &fakeWorkspace{}}
		m := New(Deps{
			Runtime: &fakeRuntime{}, Agents: fakeAgents{}, Workspace: ws, Store: st,
			Messenger: &fakeMessenger{}, Chat: launcher, Lifecycle: &fakeLCM{store: st},
			DataDir: "/ao-test-data", LookPath: func(string) (string, error) { return "/bin/true", nil },
		})
		deferred := deferredBackground(m)
		heldDuringRollback := false
		ws.onDestroy = func() { heldDuringRollback = !workspaceGateFree(m, chatTestProject) }

		cfg := asyncChatSpawnConfig("do the thing")
		cfg.Async = async
		_, _, _, err := m.Spawn(context.Background(), cfg)
		for _, work := range *deferred {
			work()
		}
		if !async && !errors.Is(err, ErrChatController) {
			t.Fatalf("spawn error = %v, want ErrChatController", err)
		}
		if ws.destroyed == 0 {
			t.Fatalf("async=%v: the failed launch left its worktree behind", async)
		}
		if !heldDuringRollback {
			t.Fatalf("async=%v: the worktree was torn down without the workspace gate", async)
		}
		if !workspaceGateFree(m, chatTestProject) {
			t.Fatalf("async=%v: the workspace gate is still held after the failed spawn", async)
		}
	}
}
