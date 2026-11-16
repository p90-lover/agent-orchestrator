package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// hostToken returns the capability of the chat's current host. Each host makes
// its own, so a different value is a different host.
func hostToken(t *testing.T, start ports.ChatStartConfig) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(start.DataDir, "chat-hosts", string(start.SessionID), "host.json"))
	if err != nil {
		t.Fatalf("read host descriptor: %v", err)
	}
	var descriptor struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &descriptor); err != nil || descriptor.Token == "" {
		t.Fatalf("host descriptor has no capability: %v", err)
	}
	return descriptor.Token
}

// abandonHostBeforeSession starts a chat's host the way the driver does and
// detaches before any session is open on it, which is what a daemon that dies
// in the middle of starting a chat leaves behind. It returns that host's token.
func abandonHostBeforeSession(t *testing.T, cfg Config, start ports.ChatStartConfig, initialize bool) string {
	t.Helper()
	driver := New(cfg, nil)
	launch := LaunchConfig{
		SessionID: start.SessionID, DataDir: start.DataDir, WorkspacePath: start.WorkspacePath,
		Permissions: start.Permissions, ProviderScopeID: start.ProviderScopeID,
	}
	proc, err := driver.connectProcess(context.Background(), launch, nil)
	if err != nil {
		t.Fatalf("start the host: %v", err)
	}
	if initialize {
		conv, _, _, err := driver.initialize(context.Background(), launch, proc)
		if err != nil {
			t.Fatalf("initialize: %v", err)
		}
		_ = conv.Close()
	} else {
		_ = proc.stop()
	}
	return hostToken(t, start)
}

// A daemon that dies between starting a chat's host and opening its session
// leaves a host that holds no session. Every later start or resume attached to
// it, found nothing to resume and gave up as "inconclusive" without replacing
// it, so the chat stayed unreachable until someone killed the host by hand.
func TestPersistentACPReplacesAHostThatNeverOpenedASession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialized bool
		resume      bool
	}{
		{"start over an attached-only host", false, false},
		{"start over an initialized host", true, false},
		{"resume over an initialized host", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, start := restartFixture(t, map[string]string{})
			// A failing run must not leave the host and its provider behind.
			t.Cleanup(func() {
				_ = persistenthost.Shutdown(context.Background(), start.DataDir, string(start.SessionID))
			})
			providerConversationID := ""
			if tc.resume {
				earlier, err := New(cfg, nil).Start(context.Background(), start)
				if err != nil {
					t.Fatalf("earlier start: %v", err)
				}
				providerConversationID = earlier.ProviderConversationID()
				if err := earlier.(ports.ChatProviderTerminator).Terminate(); err != nil {
					t.Fatalf("stop the earlier host: %v", err)
				}
			}
			abandoned := abandonHostBeforeSession(t, cfg, start, tc.initialized)

			var conv ports.ChatConversation
			var err error
			if tc.resume {
				conv, err = New(cfg, nil).Resume(context.Background(), ports.ChatResumeConfig{
					SessionID: start.SessionID, DataDir: start.DataDir, WorkspacePath: start.WorkspacePath,
					ProviderScopeID: start.ProviderScopeID, ProviderConversationID: providerConversationID,
					Permissions: start.Permissions,
				})
			} else {
				conv, err = New(cfg, nil).Start(context.Background(), start)
			}
			if err != nil {
				t.Fatalf("over a host that holds no session: %v", err)
			}
			defer func() { _ = conv.(ports.ChatProviderTerminator).Terminate() }()
			if hostToken(t, start) == abandoned {
				t.Fatal("the host that holds no session was adopted, not replaced")
			}
		})
	}
}

// The host's relay wraps a prompt error's data to carry its own event id, so
// the agent's "I no longer hold this session" (-32002) reached the driver with
// the session id one level down. It was not recognized, and the chat failed
// every turn on the dead session instead of reopening it.
func TestPersistentACPPromptOnDroppedSessionReopensIt(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.log")
	cfg, start := restartFixture(t, map[string]string{
		"AO_TEST_PERSISTENT_ACP_SESSION_GONE": "1", "AO_TEST_PERSISTENT_ACP_CALLS": calls,
	})
	conv, err := New(cfg, nil).Start(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.(ports.ChatProviderTerminator).Terminate() }()
	run := func(text string) (ports.ChatEvent, ports.ChatControllerState) {
		t.Helper()
		ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: text})
		if err != nil {
			t.Fatalf("SendTurn %q: %v", text, err)
		}
		if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
			t.Fatalf("StartDeferredTurn %q: %v", text, err)
		}
		completed, idle := awaitTurnCompleted(t, conv.Events(), ref.ProviderTurnID)
		// The host accepts the next prompt only once this result is acknowledged.
		if err := conv.(ports.ChatProviderEventAcknowledger).AcknowledgeProviderEvent(context.Background(), completed.ProviderEventID); err != nil {
			t.Fatalf("acknowledge %q: %v", text, err)
		}
		return completed, idle
	}

	completed, idle := run("first")
	if completed.TurnState != domain.TurnStateFailed {
		t.Fatalf("prompt on a dropped session = %#v, want a failed turn", completed)
	}
	if idle != ports.ChatControllerRecovering {
		t.Fatalf("controller after the agent dropped the session = %q, want recovering", idle)
	}
	completed, idle = run("follow-up")
	if completed.TurnState != domain.TurnStateCompleted || idle != ports.ChatControllerReady {
		t.Fatalf("follow-up = %#v then %q, want completed then ready", completed, idle)
	}
	recorded, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "session/resume") {
		t.Fatalf("provider calls = %q, want the dropped session reopened with session/resume", recorded)
	}
}

func restartFixture(t *testing.T, env map[string]string) (Config, ports.ChatStartConfig) {
	t.Helper()
	env["AO_TEST_PERSISTENT_ACP_PROVIDER"] = "1"
	cfg := Config{
		Harness: domain.HarnessOMP,
		Launch: func(context.Context, LaunchConfig) (Launch, error) {
			return Launch{Command: os.Args[0], Args: []string{"-test.run=TestPersistentACPProviderHelper"}, Env: env}, nil
		},
	}
	start := ports.ChatStartConfig{
		SessionID: "restart-test", DataDir: t.TempDir(), WorkspacePath: t.TempDir(),
		ProviderScopeID: "scope", Permissions: ports.PermissionModeDefault,
	}
	return cfg, start
}

func TestPersistentACPFailedPromptAcknowledgementAllowsNextTurn(t *testing.T) {
	cfg, start := restartFixture(t, map[string]string{"AO_TEST_PERSISTENT_ACP_ERROR": "1"})
	conv, err := New(cfg, nil).Start(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conv.(ports.ChatProviderTerminator).Terminate() }()
	_ = nextEvent(t, conv.Events())
	for _, want := range []domain.TurnState{domain.TurnStateFailed, domain.TurnStateCompleted} {
		ref, err := conv.SendTurn(context.Background(), ports.ChatUserMessage{Text: "run"})
		if err != nil {
			t.Fatal(err)
		}
		if err := conv.(ports.ChatDeferredTurnStarter).StartDeferredTurn(ref.ProviderTurnID); err != nil {
			t.Fatal(err)
		}
		completed, ready := false, false
		for !ready {
			event := nextEvent(t, conv.Events())
			switch event.Kind {
			case ports.ChatEventTurnCompleted:
				if event.TurnState != want || event.ProviderEventID == "" {
					t.Fatalf("completion = %#v, want %s with a durable receipt", event, want)
				}
				if err := conv.(ports.ChatProviderEventAcknowledger).AcknowledgeProviderEvent(context.Background(), event.ProviderEventID); err != nil {
					t.Fatal(err)
				}
				completed = true
			case ports.ChatEventControllerState:
				// TurnCompleted is emitted before the driver releases activeTurn.
				// ControllerReady is the barrier after which the next turn is valid.
				ready = completed && event.ControllerState == ports.ChatControllerReady
			}
		}
	}
}

func TestPersistentACPSetupFailureReleasesNewHost(t *testing.T) {
	env := map[string]string{"AO_TEST_PERSISTENT_ACP_BAD_SETUP": "1"}
	cfg, start := restartFixture(t, env)
	if _, err := New(cfg, nil).Start(context.Background(), start); err == nil {
		t.Fatal("invalid session/new response accepted")
	}
	descriptor := filepath.Join(start.DataDir, "chat-hosts", string(start.SessionID), "host.json")
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err := os.Stat(descriptor)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed setup left a host descriptor: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	delete(env, "AO_TEST_PERSISTENT_ACP_BAD_SETUP")
	conv, err := New(cfg, nil).Start(context.Background(), start)
	if err != nil {
		t.Fatalf("fresh retry: %v", err)
	}
	defer func() { _ = conv.(ports.ChatProviderTerminator).Terminate() }()
}

func TestPersistentACPAdoptionRetainsLaunchPermissionFence(t *testing.T) {
	cfg, start := restartFixture(t, map[string]string{})
	cfg.ValidateTurnSettings = func(initial ports.PermissionMode, settings ports.ChatTurnSettings) error {
		if ports.NormalizePermissionMode(initial) != ports.NormalizePermissionMode(settings.Approval) {
			return errors.New("approval is fixed at launch")
		}
		return nil
	}
	conv, err := New(cfg, nil).Start(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Close(); err != nil {
		t.Fatal(err)
	}
	resume := ports.ChatResumeConfig{
		SessionID: start.SessionID, DataDir: start.DataDir, WorkspacePath: start.WorkspacePath,
		ProviderScopeID: start.ProviderScopeID, ProviderConversationID: conv.ProviderConversationID(),
		Permissions: ports.PermissionModeBypassPermissions,
	}
	if _, err := New(cfg, nil).Resume(context.Background(), resume); !errors.Is(err, ports.ErrChatRecoveryInconclusive) {
		t.Fatalf("changed launch policy = %v, want preserved but refused", err)
	}
	resume.Permissions = start.Permissions
	adopted, err := New(cfg, nil).Resume(context.Background(), resume)
	if err != nil {
		t.Fatalf("original policy could not adopt preserved host: %v", err)
	}
	defer func() { _ = adopted.(ports.ChatProviderTerminator).Terminate() }()
}

func TestPersistentACPRequiresDurableSessionIdentity(t *testing.T) {
	driver := New(Config{Harness: domain.HarnessClaudeCode, Launch: func(context.Context, LaunchConfig) (Launch, error) {
		t.Fatal("invalid session identity invoked launch side effects")
		return Launch{}, nil
	}}, nil)
	for _, cfg := range []ports.ChatStartConfig{
		{WorkspacePath: t.TempDir()},
		{WorkspacePath: t.TempDir(), DataDir: t.TempDir(), SessionID: "../escape"},
	} {
		if _, err := driver.Start(context.Background(), cfg); err == nil {
			t.Fatal("invalid session identity accepted")
		}
	}
}
