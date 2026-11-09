package codexappserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestLiveRTKHookRewritesCodexShellCommands drives a real `codex app-server`
// through AO's driver with RTK's hook turned on, against a stand-in Responses
// API that asks for `git status`. It needs no account and makes no model calls,
// but it does need codex and rtk installed, so it is skipped unless
// AO_CODEX_RTK_LIVE=1:
//
//	AO_CODEX_RTK_LIVE=1 go test ./internal/adapters/chatdriver/codexappserver/ -run LiveRTK -v
func TestLiveRTKHookRewritesCodexShellCommands(t *testing.T) {
	if os.Getenv("AO_CODEX_RTK_LIVE") != "1" {
		t.Skip("set AO_CODEX_RTK_LIVE=1 to run against a real codex app-server and rtk")
	}
	bin := os.Getenv("AO_CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("codex binary %q not on PATH: %v", bin, err)
	}
	// The hook and the command it returns both call `rtk` by name.
	if _, err := exec.LookPath("rtk"); err != nil {
		t.Skipf("rtk not on PATH: %v", err)
	}

	model := newStandInResponses(t)
	home := t.TempDir()
	config := fmt.Sprintf("model = \"stand-in\"\nmodel_provider = \"stand-in\"\n\n[model_providers.stand-in]\n"+
		"name = \"stand-in\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\n",
		model.URL+"/v1")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	seedGitWorkspace(t, workspace)

	d := New(livePlugin{bin: bin}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	// The detached chat host re-executes AO's daemon binary, which a test binary
	// is not; the direct launcher builds the same argv (appServerArgs).
	d.persistent = false

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conv, err := d.Start(ctx, ports.ChatStartConfig{
		SessionID:     "ao-live-rtk",
		WorkspacePath: workspace,
		Env: map[string]string{
			"CODEX_HOME": home, EnvRTKCodexHook: "rtk hook codex", "RTK_SUPPRESS_HOOK_WARNING": "1",
			"NO_PROXY": "127.0.0.1,localhost", "HTTPS_PROXY": "", "HTTP_PROXY": "", "ALL_PROXY": "",
		},
		Permissions: ports.PermissionModeDefault,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = conv.Close() }()
	if _, err := conv.SendTurn(ctx, ports.ChatUserMessage{
		Text: "run git status", ClientMessageID: "live-rtk-1", Origin: domain.MessageOriginHuman,
	}); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	var commands []string
collect:
	for {
		select {
		case ev, ok := <-conv.Events():
			if !ok {
				t.Fatal("event stream closed before the turn completed")
			}
			switch ev.Kind {
			case ports.ChatEventActivityStarted, ports.ChatEventActivityCompleted:
				if ev.ActivityKind != domain.ActivityKindCommand {
					continue
				}
				var detail struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(ev.Detail, &detail)
				commands = append(commands, detail.Command)
			case ports.ChatEventTurnCompleted:
				break collect
			case ports.ChatEventControllerState:
				if ev.ControllerState == ports.ChatControllerStopped {
					t.Fatalf("controller stopped before the turn completed: %v", ev.Err)
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out; commands so far %q", commands)
		}
	}

	t.Logf("shell commands Codex ran: %q", commands)
	t.Logf("tool output the model read:\n%s", model.toolOutput())
	if len(commands) == 0 {
		t.Fatal("Codex ran no shell command")
	}
	// On Windows the command arrives wrapped in Codex's PowerShell invocation
	// ("...powershell.exe" -Command 'rtk git status').
	for _, command := range commands {
		if !strings.Contains(command, "rtk git status") {
			t.Fatalf("Codex ran %q, want RTK's rewrite of `git status`", command)
		}
	}
}

// standInResponses is a Responses API that answers the first request with one
// shell call, `git status`, and the next (carrying its output) with "done".
type standInResponses struct {
	*httptest.Server
	mu     sync.Mutex
	output string
}

func newStandInResponses(t *testing.T) *standInResponses {
	t.Helper()
	s := &standInResponses{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *standInResponses) toolOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

func (s *standInResponses) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/responses") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Input []struct {
			Type   string          `json:"type"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	_ = json.Unmarshal(body, &request)
	var output []string
	for _, item := range request.Input {
		if item.Type == "function_call_output" {
			var text string
			if json.Unmarshal(item.Output, &text) != nil {
				text = string(item.Output)
			}
			output = append(output, text)
		}
	}

	var item map[string]any
	if len(output) == 0 {
		name, args := "shell", map[string]any{"command": []string{"git", "status"}}
		for _, tool := range request.Tools {
			switch tool.Name {
			case "exec_command":
				name, args = tool.Name, map[string]any{"cmd": "git status"}
			case "shell_command":
				if name != "exec_command" {
					name, args = tool.Name, map[string]any{"command": "git status"}
				}
			}
		}
		arguments, _ := json.Marshal(args)
		item = map[string]any{"type": "function_call", "name": name, "arguments": string(arguments), "call_id": "call-git-status"}
	} else {
		s.mu.Lock()
		s.output = strings.Join(output, "\n")
		s.mu.Unlock()
		item = map[string]any{"type": "message", "role": "assistant", "id": "msg-done",
			"content": []any{map[string]any{"type": "output_text", "text": "done"}}}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	usage := map[string]any{"input_tokens": 1, "input_tokens_details": nil, "output_tokens": 1, "output_tokens_details": nil, "total_tokens": 2}
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp-stand-in"}},
		{"type": "response.output_item.done", "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp-stand-in", "usage": usage}},
	} {
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}
