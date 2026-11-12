package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestChatSpawnContextWindowReachesControllerAndDurableSessionOptions(t *testing.T) {
	launcher := &recordingLauncher{}
	manager, _, _ := newChatManager(launcher)
	manager.dataDir = t.TempDir()
	record, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat, AgentConfig: ports.AgentConfig{Model: "gpt-test", ContextWindow: 32768},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.started) != 1 || launcher.started[0].ContextWindow != 32768 {
		t.Fatalf("controller context=%+v", launcher.started)
	}
	restarted := &Manager{dataDir: manager.dataDir}
	options, ok := restarted.gateways.get(manager.dataDir, record.ID)
	if !ok || options.ContextWindow != 32768 || options.Enabled() {
		t.Fatalf("durable native session tuning=%+v present=%v", options, ok)
	}
}

func TestContextWindowPersistenceFailureTerminatesOnlyTheUnlaunchedSeed(t *testing.T) {
	launcher := &recordingLauncher{}
	manager, store, _ := newChatManager(launcher)
	manager.dataDir = t.TempDir()
	other := domain.SessionRecord{ID: "other-active", Metadata: domain.SessionMetadata{RuntimeHandleID: "keep-runtime"}}
	store.sessions[other.ID] = other
	// A directory at the atomic temp file path makes only tuning persistence fail.
	if err := os.Mkdir(filepath.Join(manager.dataDir, gatewayFileName+".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat, AgentConfig: ports.AgentConfig{Model: "gpt-test", ContextWindow: 32768},
	})
	if err == nil || len(launcher.started) != 0 || len(store.sessions) != 1 {
		t.Fatalf("persist failure err=%v starts=%d sessions=%d", err, len(launcher.started), len(store.sessions))
	}
	if kept := store.sessions[other.ID]; kept.IsTerminated || kept.Metadata.RuntimeHandleID != "keep-runtime" {
		t.Fatalf("unrelated session changed: %+v", kept)
	}
	if len(manager.gateways.entries) != 0 {
		t.Fatalf("failed context options remain cached: %+v", manager.gateways.entries)
	}
}

func TestContextWindowResumeUsesDurableModelBeforeValidation(t *testing.T) {
	for _, mode := range []domain.SessionMode{domain.SessionModeChat, domain.SessionModeTUI} {
		for _, projectModel := range []string{"", "unqualified-default"} {
			launcher := &recordingLauncher{}
			manager, store, runtime := newChatManager(launcher)
			manager.dataDir = t.TempDir()
			project := store.projects[string(chatTestProject)]
			project.Config.AgentConfig.Model = projectModel
			project.Config.Worker.AgentConfig.Model = projectModel
			record := domain.SessionRecord{
				ID: "context-resume", ProjectID: chatTestProject, Kind: domain.KindWorker,
				Harness: domain.HarnessOpenCode, Mode: mode,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
				Metadata: domain.SessionMetadata{Model: "openai/gpt-5.5(high)", ProviderConversationID: "thread-existing", AgentSessionID: "native-existing"},
			}
			store.sessions[record.ID] = record
			if err := manager.gateways.set(manager.dataDir, record.ID, ports.SessionGateway{
				Model: record.Metadata.Model, ContextWindow: 32768,
			}); err != nil {
				t.Fatal(err)
			}
			workspace := ports.WorkspaceInfo{Path: t.TempDir()}
			var err error
			if mode == domain.SessionModeChat {
				_, err = manager.resumeChatController(context.Background(), "resume", record, project, workspace, false, "", "")
			} else {
				_, err = manager.relaunchSessionWithPolicyAndGeneration(context.Background(), "resume", record, project, workspace, nil, false, false, "", "")
			}
			if err != nil {
				t.Fatalf("%s project model %q: %v", mode, projectModel, err)
			}
			if mode == domain.SessionModeChat && (len(launcher.started) != 1 || launcher.started[0].Model != record.Metadata.Model || launcher.started[0].ContextWindow != 32768) {
				t.Fatalf("restored controller=%+v", launcher.started)
			}
			if mode == domain.SessionModeTUI && runtime.created != 1 {
				t.Fatalf("TUI restore created %d runtimes", runtime.created)
			}
		}
	}
}

func TestSetAgentContextWindowAppliesFromTheNextResume(t *testing.T) {
	launcher := &recordingLauncher{}
	manager, store, _ := newChatManager(launcher)
	manager.dataDir = t.TempDir()
	spawned, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat, AgentConfig: ports.AgentConfig{Model: "gpt-test", ContextWindow: 32768},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAgentContextWindow(context.Background(), spawned.ID, 65536); err != nil {
		t.Fatal(err)
	}
	// Saved for the session's next start, surviving a daemon restart like the spawn's own.
	restarted := &Manager{dataDir: manager.dataDir}
	if options, ok := restarted.gateways.get(manager.dataDir, spawned.ID); !ok || options.ContextWindow != 65536 || options.Enabled() {
		t.Fatalf("saved options=%+v present=%v", options, ok)
	}
	record := store.sessions[spawned.ID]
	record.Metadata.ProviderConversationID = "thread-existing"
	_, err = manager.resumeChatController(context.Background(), "resume", record,
		store.projects[string(chatTestProject)], ports.WorkspaceInfo{Path: t.TempDir()}, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if last := launcher.started[len(launcher.started)-1]; last.ContextWindow != 65536 {
		t.Fatalf("resumed controller context=%d, want 65536", last.ContextWindow)
	}

	// Zero returns the agent to its client default: nothing is kept for the session.
	if _, err := manager.SetAgentContextWindow(context.Background(), spawned.ID, 0); err != nil {
		t.Fatal(err)
	}
	if options, ok := manager.gateways.get(manager.dataDir, spawned.ID); ok {
		t.Fatalf("cleared window still saved: %+v", options)
	}
}

func TestSetAgentContextWindowRefusesWhatTheResumeWouldRefuse(t *testing.T) {
	manager, store, _ := newChatManager(&recordingLauncher{})
	manager.dataDir = t.TempDir()
	record := domain.SessionRecord{
		ID: "agy-session", ProjectID: chatTestProject, Kind: domain.KindWorker,
		Harness: domain.HarnessAgy, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	store.sessions[record.ID] = record
	_, err := manager.SetAgentContextWindow(context.Background(), record.ID, 32768)
	var refusal *ContextWindowRefusal
	if !errors.As(err, &refusal) || refusal.Reason == "" {
		t.Fatalf("unsupported harness err=%v", err)
	}
	if options, ok := manager.gateways.get(manager.dataDir, record.ID); ok {
		t.Fatalf("refused window was saved: %+v", options)
	}
	if _, err := manager.SetAgentContextWindow(context.Background(), "missing", 32768); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session err=%v", err)
	}
	record.ID, record.Harness, record.IsTerminated = "ended", domain.HarnessCodex, true
	store.sessions[record.ID] = record
	if _, err := manager.SetAgentContextWindow(context.Background(), record.ID, 32768); !errors.Is(err, ErrTerminated) {
		t.Fatalf("terminated session err=%v", err)
	}
}

func TestSpawnUnsupportedContextWindowCreatesNoSession(t *testing.T) {
	launcher := &recordingLauncher{}
	manager, store, _ := newChatManager(launcher)
	manager.dataDir = t.TempDir()
	_, _, _, err := manager.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessAgy,
		RequestedMode: domain.SessionModeChat, AgentConfig: ports.AgentConfig{ContextWindow: 32768},
	})
	if err == nil || len(launcher.started) != 0 || len(store.sessions) != 0 {
		t.Fatalf("unsupported context err=%v starts=%d sessions=%d", err, len(launcher.started), len(store.sessions))
	}
}
