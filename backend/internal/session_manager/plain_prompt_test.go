package sessionmanager

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// A plain worker is a bare assistant: it launches with no AO system prompt, the
// choice is persisted, and every later rebuild from the record (restore, agent
// switch, chat respawn) stays plain. A default worker keeps AO's prompt.
func TestSpawnWorker_PlainPromptSkipsAOSystemPrompt(t *testing.T) {
	for _, plain := range []bool{false, true} {
		st := newFakeStore()
		st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: t.TempDir(), Config: testRoleAgents()}
		agent := &recordingAgent{}
		lookPath := func(string) (string, error) { return "/bin/true", nil }
		m := New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: agent}, Workspace: &fakeWorkspace{}, Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}, LookPath: lookPath})

		s, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "hello", PlainPrompt: plain})
		if err != nil {
			t.Fatalf("plain=%v spawn: %v", plain, err)
		}
		launched := agent.lastLaunch.SystemPrompt
		rec := st.sessions[s.ID]
		if rec.Metadata.PlainPrompt != plain {
			t.Fatalf("plain=%v: persisted PlainPrompt=%v", plain, rec.Metadata.PlainPrompt)
		}
		rebuilt, err := m.buildSystemPrompt(ctx, rec.Kind, rec.ProjectID, rec.Metadata.PlainPrompt)
		if err != nil {
			t.Fatalf("plain=%v rebuild: %v", plain, err)
		}
		if plain {
			if launched != "" || rebuilt != "" {
				t.Fatalf("plain worker got AO's system prompt: launch=%q rebuild=%q", launched, rebuilt)
			}
			if agent.lastLaunch.Prompt != "hello" {
				t.Fatalf("plain worker prompt = %q, want the user's text only", agent.lastLaunch.Prompt)
			}
			continue
		}
		if !strings.Contains(launched, "Using the ao CLI") || !strings.Contains(rebuilt, "Using the ao CLI") {
			t.Fatalf("default worker lost AO's prompt:\n%s", launched)
		}
	}
}
