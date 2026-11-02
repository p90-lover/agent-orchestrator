package sessionmanager

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// An exited chat whose controller ended without stopping its provider host (the
// daemon restarted and nothing reattached) still has that host and its agent
// running. Exiting it again must stop the host, and still report that the agent
// had already exited.
func TestExitAgentOnExitedChatStopsLeftoverHost(t *testing.T) {
	launcher := &recordingLauncher{}
	m, st, rt := newChatManager(launcher)
	st.sessions["mer-1"] = domain.SessionRecord{
		ID: "mer-1", ProjectID: chatTestProject, Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity: domain.Activity{State: domain.ActivityExited},
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/mer-1", Branch: "ao/mer-1/root"},
	}

	_, err := m.ExitAgent(ctx, "mer-1")
	if !errors.Is(err, ErrAgentExited) {
		t.Fatalf("ExitAgent error = %v, want ErrAgentExited", err)
	}
	if !reflect.DeepEqual(launcher.stopped, []domain.SessionID{"mer-1"}) {
		t.Fatalf("leftover chat host stops = %v, want [mer-1]", launcher.stopped)
	}
	if rt.destroyed != 0 {
		t.Fatalf("a chat session's exit touched the terminal runtime: %d", rt.destroyed)
	}
	if got := st.sessions["mer-1"]; got.IsTerminated || got.Activity.State != domain.ActivityExited {
		t.Fatalf("session changed: %+v", got)
	}
}

func TestExitAgentOnExitedTerminalSessionLeavesChatAlone(t *testing.T) {
	launcher := &recordingLauncher{}
	m, st, _ := newChatManager(launcher)
	st.sessions["mer-2"] = domain.SessionRecord{
		ID: "mer-2", ProjectID: chatTestProject, Kind: domain.KindWorker,
		Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
		Activity: domain.Activity{State: domain.ActivityExited},
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/mer-2", Branch: "ao/mer-2", RuntimeHandleID: "tmux-mer-2"},
	}

	if _, err := m.ExitAgent(ctx, "mer-2"); !errors.Is(err, ErrAgentExited) {
		t.Fatalf("ExitAgent error = %v, want ErrAgentExited", err)
	}
	if len(launcher.stopped) != 0 {
		t.Fatalf("a terminal session stopped a chat host: %v", launcher.stopped)
	}
}
