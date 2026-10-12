package codexappserver

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestMCPServersAreAddedToThreadStartAndResumeConfig(t *testing.T) {
	servers := []ports.ChatMCPServerConfig{
		{Name: "coding-tools", Type: "http", URL: "http://127.0.0.1:41234/mcp/ws-1",
			Headers: map[string]string{"Authorization": "Bearer secret-token"}},
		{Name: "local", Type: "stdio", Command: "node", Args: []string{"server.js"},
			Env: map[string]string{"TOKEN": "x"}},
	}
	for _, resume := range []bool{false, true} {
		d, srv := newTestDriver(t)
		var conv ports.ChatConversation
		var err error
		method := "thread/start"
		if resume {
			method = "thread/resume"
			conv, err = d.Resume(context.Background(), ports.ChatResumeConfig{
				SessionID: "mcp-resume", WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1",
				Effort: "low", MCPServers: servers,
			})
		} else {
			conv, err = d.Start(context.Background(), ports.ChatStartConfig{
				SessionID: "mcp-start", WorkspacePath: t.TempDir(), Effort: "low", MCPServers: servers,
			})
		}
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		request := srv.awaitFrame(func(f frame) bool { return f.Method == method })
		var params struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"model_reasoning_effort": "low",
			// One dotted key per server, so the user's own mcp_servers table is kept.
			"mcp_servers.coding-tools": map[string]any{
				"url":          "http://127.0.0.1:41234/mcp/ws-1",
				"http_headers": map[string]any{"Authorization": "Bearer secret-token"},
			},
			"mcp_servers.local": map[string]any{
				"command": "node", "args": []any{"server.js"}, "env": map[string]any{"TOKEN": "x"},
			},
		}
		if !reflect.DeepEqual(params.Config, want) {
			t.Fatalf("%s config=%#v\nwant %#v", method, params.Config, want)
		}
		if _, ok := params.Config["mcp_servers"]; ok {
			t.Fatalf("%s replaced the whole mcp_servers table", method)
		}
		if err := conv.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoMCPServersLeaveTheConfigUntouched(t *testing.T) {
	config := map[string]any{}
	if err := addMCPServerConfig(config, nil); err != nil || len(config) != 0 {
		t.Fatalf("config=%v err=%v", config, err)
	}
}

func TestCodexRefusesMCPServersItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		server ports.ChatMCPServerConfig
		want   string
	}{
		{ports.ChatMCPServerConfig{Name: "events", Type: "sse", URL: "http://127.0.0.1:1/sse"}, "does not support SSE"},
		{ports.ChatMCPServerConfig{Name: "web", Type: "websocket", URL: "ws://127.0.0.1:1"}, "unsupported codex MCP transport"},
		{ports.ChatMCPServerConfig{Name: "a.b", Type: "http", URL: "http://127.0.0.1:1"}, "not a plain identifier"},
		{ports.ChatMCPServerConfig{Name: "web", Type: "http"}, "requires a URL"},
		{ports.ChatMCPServerConfig{Name: "cli", Type: "stdio"}, "requires a command"},
	} {
		err := addMCPServerConfig(map[string]any{}, []ports.ChatMCPServerConfig{tc.server})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%+v: err=%v, want %q", tc.server, err, tc.want)
		}
	}
	d, _ := newTestDriver(t)
	_, err := d.Start(context.Background(), ports.ChatStartConfig{
		SessionID: "mcp-sse", WorkspacePath: t.TempDir(),
		MCPServers: []ports.ChatMCPServerConfig{{Name: "events", Type: "sse", URL: "http://127.0.0.1:1/sse"}},
	})
	if err == nil || !strings.Contains(err.Error(), "SSE") {
		t.Fatalf("Start with an SSE server: err=%v", err)
	}
}
