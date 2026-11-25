package piacp

import (
	"bytes"
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

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/pi"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestMain(m *testing.M) {
	// The driver's persistent chat host re-runs this executable as `chat-host`, as it runs ao.
	if len(os.Args) >= 8 && os.Args[1] == "chat-host" {
		if os.Args[5] != string(persistenthost.ProtocolACP) || os.Args[7] != "--" {
			os.Exit(2)
		}
		err := persistenthost.Run(context.Background(), persistenthost.Config{
			SessionID: os.Args[2], DataDir: os.Args[3], Workdir: os.Args[4],
			Env: os.Environ(), Argv: os.Args[8:], Protocol: persistenthost.ProtocolACP,
			OwnershipFingerprint: os.Args[6],
		})
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	runFakeAOIfAsked()
	os.Exit(m.Run())
}

// runFakeAOIfAsked makes the test binary stand in for `ao hooks pi tool-call`
// when AO's managed Pi extension runs it inside the pi-acp daemon: it posts the
// tool call to the live test's stand-in daemon route, which hands it to the
// conversation, and prints the answer the way the real hook does.
func runFakeAOIfAsked() {
	target := os.Getenv("AO_TEST_TOOL_APPROVAL_URL")
	if target == "" || len(os.Args) < 4 || strings.Join(os.Args[1:4], " ") != "hooks pi tool-call" {
		return
	}
	stdin, _ := io.ReadAll(os.Stdin)
	request, _ := http.NewRequest(http.MethodPost, target, bytes.NewReader(stdin))
	request.Header.Set("Authorization", "Bearer "+os.Getenv(envApprovalToken))
	answer := map[string]any{"allow": false, "reason": "the stand-in AO could not ask"}
	if response, err := http.DefaultClient.Do(request); err == nil {
		_ = json.NewDecoder(response.Body).Decode(&answer)
		_ = response.Body.Close()
	}
	_ = json.NewEncoder(os.Stdout).Encode(answer)
	os.Exit(0)
}

// TestLivePiAsksTheChatBeforeItsShell drives a real pi-acp through AO's driver
// in accept edits (the chat's "Ask for approval"), with AO's managed Pi
// extension installed in the workspace, against a stand-in Chat Completions API
// that asks for `git status`. It checks that the shell call reaches the chat as
// an approval request and that rejecting it keeps the command from running. It
// makes no model calls but needs pi-acp and pi installed, so it is skipped
// unless AO_PI_ASK_LIVE=1:
//
//	AO_PI_ASK_LIVE=1 go test ./internal/adapters/chatdriver/piacp/ -run LivePiAsks -v
func TestLivePiAsksTheChatBeforeItsShell(t *testing.T) {
	if os.Getenv("AO_PI_ASK_LIVE") != "1" {
		t.Skip("set AO_PI_ASK_LIVE=1 to run against a real pi-acp")
	}
	binary, err := ResolveBinary(context.Background())
	if err != nil {
		t.Skipf("pi-acp not installed: %v", err)
	}
	executable = os.Executable // the test binary, standing in for ao
	// pi-acp's sockets live under the data dir, and a Unix socket path is limited to about
	// 108 characters: a deep temp folder makes the daemon exit before it listens.
	root := t.TempDir()
	if short := os.Getenv("AO_PI_ASK_ROOT"); short != "" {
		root = filepath.Join(short, fmt.Sprint(time.Now().UnixNano()%1e6))
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	workspace := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	dataDir := filepath.Join(root, "data")
	for _, dir := range []string{workspace, agentDir, dataDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := pi.New().GetAgentHooks(context.Background(), ports.WorkspaceHookConfig{WorkspacePath: workspace}); err != nil {
		t.Fatalf("install AO's Pi extension: %v", err)
	}
	model := newPiStandIn(t)
	writeJSON(t, filepath.Join(agentDir, "models.json"), map[string]any{"providers": map[string]any{"standin": map[string]any{
		"baseUrl": model.URL + "/v1", "api": "openai-completions", "apiKey": "AO_TEST_STANDIN_KEY",
		"models": []map[string]any{{"id": "m", "name": "m", "reasoning": false, "input": []string{"text"},
			"contextWindow": 100000, "maxTokens": 1000}},
	}}})
	writeJSON(t, filepath.Join(agentDir, "settings.json"), map[string]any{"defaultProvider": "standin", "defaultModel": "m"})

	var conversation ports.ChatConversation
	var ready sync.WaitGroup
	ready.Add(1)
	approvals := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready.Wait()
		var call struct {
			ToolCallID string          `json:"tool_call_id"`
			ToolName   string          `json:"tool_name"`
			Input      json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		var input struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(call.Input, &input)
		result, err := conversation.(ports.ChatToolApprovalRequester).RequestToolApproval(r.Context(), ports.ChatToolApprovalRequest{
			Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ToolCallID: call.ToolCallID,
			ToolName: call.ToolName, Kind: "execute", Title: input.Command, Input: call.Input,
		})
		answer := map[string]any{"allow": result.Allowed, "reason": result.Message}
		if err != nil {
			answer = map[string]any{"allow": false, "reason": err.Error()}
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(approvals.Close)

	env := map[string]string{
		"PI_CODING_AGENT_DIR": agentDir, "AO_TEST_STANDIN_KEY": "stand-in",
		"AO_TEST_TOOL_APPROVAL_URL": approvals.URL,
		"NO_PROXY":                  "127.0.0.1,localhost", "HTTPS_PROXY": "", "HTTP_PROXY": "", "ALL_PROXY": "",
	}
	t.Cleanup(func() {
		stop := exec.Command(binary, "--daemon-stop")
		stop.Env = append(os.Environ(), "PI_ACP_SOCKET_DIR="+filepath.Join(dataDir, "pi-acp", "live-pi-ask", "run"))
		_ = stop.Run()
	})
	driver := New(fakePiPlugin{status: ports.AgentAuthStatusAuthorized}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conversation, err = driver.Start(ctx, ports.ChatStartConfig{
		SessionID: "live-pi-ask", DataDir: dataDir, WorkspacePath: workspace,
		Model: "standin/m", Permissions: ports.PermissionModeAcceptEdits, Env: env,
	})
	ready.Done()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if terminator, ok := conversation.(ports.ChatProviderTerminator); ok {
			_ = terminator.Terminate()
		}
	}()

	ref, err := conversation.SendTurn(ctx, ports.ChatUserMessage{
		Text: "run git status", ClientMessageID: "live-pi-ask-1", Origin: domain.MessageOriginHuman,
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	if starter, ok := conversation.(ports.ChatDeferredTurnStarter); ok {
		if err := starter.StartDeferredTurn(ref.ProviderTurnID); err != nil {
			t.Fatalf("StartDeferredTurn: %v", err)
		}
	}
	var requests, details []string
	for done := false; !done; {
		select {
		case event, ok := <-conversation.Events():
			if !ok {
				t.Fatal("event stream closed before the turn completed")
			}
			switch event.Kind {
			case ports.ChatEventApprovalRequested:
				requests = append(requests, fmt.Sprintf("%s %s %s", event.ActivityKind, event.Summary, event.Detail))
				reject := ""
				for _, option := range event.Decisions {
					if option.Kind == ports.ChatDecisionRejectOnce {
						reject = option.ID
					}
				}
				if err := conversation.ResolveRequest(ctx, event.RequestID, ports.ChatDecision{ID: reject}); err != nil {
					t.Fatalf("ResolveRequest: %v", err)
				}
			case ports.ChatEventActivityStarted, ports.ChatEventActivityCompleted:
				details = append(details, fmt.Sprintf("%s %s %s", event.Kind, event.ActivityKind, event.Detail))
			case ports.ChatEventTurnCompleted:
				done = true
			case ports.ChatEventControllerState:
				if event.ControllerState == ports.ChatControllerStopped {
					t.Fatalf("controller stopped before the turn completed: %v", event.Err)
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out; requests %q, activity:\n%s", requests, strings.Join(details, "\n"))
		}
	}
	t.Logf("approval requests: %q", requests)
	t.Logf("activity:\n%s", strings.Join(details, "\n"))
	output := model.toolOutput()
	t.Logf("tool output the model read:\n%s", output)
	if len(requests) != 1 || !strings.Contains(requests[0], "git status") {
		t.Fatalf("approval requests = %q, want one for git status", requests)
	}
	if !model.askedForShell() || strings.Contains(output, "not a git repository") || strings.Contains(output, "On branch") {
		t.Fatalf("git status ran although the chat rejected it:\n%s", output)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// piStandIn is a Chat Completions API that answers a request offering the bash
// tool with one call, `git status`, and the next (carrying its result) with
// "done".
type piStandIn struct {
	*httptest.Server
	mu     sync.Mutex
	output string
	shell  bool
}

func newPiStandIn(t *testing.T) *piStandIn {
	t.Helper()
	s := &piStandIn{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *piStandIn) toolOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.output
}

func (s *piStandIn) askedForShell() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shell
}

func (s *piStandIn) serve(w http.ResponseWriter, r *http.Request) {
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
			results = append(results, string(message.Content))
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
		arguments, _ := json.Marshal(map[string]string{"command": "git status"})
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
