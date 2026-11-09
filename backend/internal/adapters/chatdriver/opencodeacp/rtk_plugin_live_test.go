package opencodeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/opencode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestLiveRTKPluginRewritesOpenCodeShellCommands drives a real `opencode acp`
// through AO's driver with RTK's plugin listed in the session overlay, against
// a stand-in Chat Completions API that asks for `git status`. It needs no
// account and makes no model calls, but it does need OpenCode 1.x, rtk on PATH
// and the plugin file Coding Tools writes (rtk.cjs, CODING_TOOLS_RTK_OPENCODE_PLUGIN),
// so it is skipped unless AO_OPENCODE_RTK_LIVE=1:
//
//	AO_OPENCODE_RTK_LIVE=1 AO_OPENCODE_RTK_PLUGIN=<dir>/rtk.ts \
//	  go test ./internal/adapters/chatdriver/opencodeacp/ -run LiveRTK -v
//
// OpenCode runs with its own folders in AO_OPENCODE_RTK_HOME (default: a temp
// folder), never the person's: in a new one OpenCode first installs its plugin
// package, which can take minutes, so a reused folder is quicker.
func TestLiveRTKPluginRewritesOpenCodeShellCommands(t *testing.T) {
	if os.Getenv("AO_OPENCODE_RTK_LIVE") != "1" {
		t.Skip("set AO_OPENCODE_RTK_LIVE=1 to run against a real OpenCode and rtk")
	}
	plugin := os.Getenv("AO_OPENCODE_RTK_PLUGIN")
	if plugin == "" {
		t.Skip("set AO_OPENCODE_RTK_PLUGIN to the plugin file Coding Tools writes")
	}
	// The plugin and the command it returns both call `rtk` by name.
	if _, err := exec.LookPath("rtk"); err != nil {
		t.Skipf("rtk not on PATH: %v", err)
	}
	home := os.Getenv("AO_OPENCODE_RTK_HOME")
	if home == "" {
		home = t.TempDir()
	}

	model := newStandInChat(t)
	overlay, _ := json.Marshal(map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   "standin/m",
		"provider": map[string]any{"standin": map[string]any{
			"npm": "@ai-sdk/openai-compatible", "name": "Stand-in",
			"options": map[string]any{"baseURL": model.URL + "/v1", "apiKey": "stand-in"},
			"models": map[string]any{"m": map[string]any{
				"name": "m", "tool_call": true, "limit": map[string]any{"context": 100000, "output": 1000},
			}},
		}},
	})
	workspace := t.TempDir()
	seedGitWorkspace(t, workspace)

	driver := New(opencode.New(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	conversation, err := driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "live-opencode-rtk", DataDir: t.TempDir(), WorkspacePath: workspace,
		Model: "standin/m", Permissions: ports.PermissionModeAuto,
		Env: map[string]string{
			"OPENCODE_CONFIG_CONTENT": string(overlay), EnvRTKOpenCodePlugin: plugin,
			"XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"),
			"XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_STATE_HOME": filepath.Join(home, "state"),
			"RTK_SUPPRESS_HOOK_WARNING": "1",
			"NO_PROXY":                  "127.0.0.1,localhost", "HTTPS_PROXY": "", "HTTP_PROXY": "", "ALL_PROXY": "",
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer conversation.(ports.ChatProviderTerminator).Terminate()

	ref, err := conversation.SendTurn(ctx, ports.ChatUserMessage{
		Text: "run git status", ClientMessageID: "live-rtk-1", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if starter, ok := conversation.(ports.ChatDeferredTurnStarter); ok {
		if err := starter.StartDeferredTurn(ref.ProviderTurnID); err != nil {
			t.Fatalf("StartDeferredTurn: %v", err)
		}
	}

	var details []string
collect:
	for {
		select {
		case event, ok := <-conversation.Events():
			if !ok {
				t.Fatal("event stream closed before the turn completed")
			}
			switch event.Kind {
			case ports.ChatEventActivityStarted, ports.ChatEventActivityCompleted:
				details = append(details, fmt.Sprintf("%s %s %s", event.Kind, event.ActivityKind, event.Detail))
			case ports.ChatEventTurnCompleted:
				if acknowledger, ok := conversation.(ports.ChatProviderEventAcknowledger); ok {
					_ = acknowledger.AcknowledgeProviderEvent(context.Background(), event.ProviderEventID)
				}
				break collect
			case ports.ChatEventControllerState:
				if event.ControllerState == ports.ChatControllerStopped {
					t.Fatalf("controller stopped before the turn completed: %v", event.Err)
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out; activity so far:\n%s", strings.Join(details, "\n"))
		}
	}

	output := model.toolOutput()
	t.Logf("activity:\n%s", strings.Join(details, "\n"))
	t.Logf("tool output the model read:\n%s", output)
	if !model.askedForShell() {
		t.Fatal("OpenCode offered the stand-in no bash tool")
	}
	if strings.TrimSpace(output) == "" {
		t.Fatal("the model read no output from git status")
	}
	// The command OpenCode ran, as its bash tool reports it to ACP. OpenCode
	// 1.x sends the arguments the plugin rewrote with the finished call.
	ran := false
	for _, detail := range details {
		if strings.Contains(detail, "rtk git status") {
			ran = true
		}
	}
	// git's own long form ("On branch ...", "Changes to be committed") is what
	// an unhooked `git status` prints; rtk's is a few compact lines.
	if !ran || strings.Contains(output, "Changes to be committed") {
		t.Fatalf("OpenCode ran git status without RTK (rewritten command seen: %v):\n%s", ran, output)
	}
}

// standInChat is a Chat Completions API that answers a request offering the
// bash tool with one call, `git status`, and the next (carrying its output)
// with "done". Requests without tools (OpenCode's title) get "done" too.
type standInChat struct {
	*httptest.Server
	mu     sync.Mutex
	output string
	shell  bool
}

func newStandInChat(t *testing.T) *standInChat {
	t.Helper()
	s := &standInChat{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *standInChat) toolOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

func (s *standInChat) askedForShell() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shell
}

func (s *standInChat) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)
	bash := false
	for _, tool := range request.Tools {
		bash = bash || tool.Function.Name == "bash"
	}
	var results []string
	for _, message := range request.Messages {
		if message.Role == "tool" {
			var text string
			if json.Unmarshal(message.Content, &text) != nil {
				text = string(message.Content)
			}
			results = append(results, text)
		}
	}

	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-stand-in", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	var chunks []map[string]any
	if bash && len(results) == 0 {
		s.mu.Lock()
		s.shell = true
		s.mu.Unlock()
		arguments, _ := json.Marshal(map[string]string{"command": "git status", "description": "Shows working tree status"})
		chunks = []map[string]any{
			chunk(map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"index": 0, "id": "call-git-status", "type": "function",
				"function": map[string]any{"name": "bash", "arguments": string(arguments)},
			}}}, nil),
			chunk(map[string]any{}, "tool_calls"),
		}
	} else {
		if len(results) > 0 {
			s.mu.Lock()
			s.output = strings.Join(results, "\n")
			s.mu.Unlock()
		}
		chunks = []map[string]any{chunk(map[string]any{"role": "assistant", "content": "done"}, nil), chunk(map[string]any{}, "stop")}
	}
	chunks[len(chunks)-1]["usage"] = map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}

	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		data, _ := json.Marshal(c)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// seedGitWorkspace makes dir a git repository with one staged file, so
// `git status` has something to report.
func seedGitWorkspace(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "a.txt"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
