package controllers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const mcpSpawnBody = `{"kind":"worker","harness":"codex","mode":"chat","prompt":"hello","mcpServers":[` +
	`{"name":"coding-tools","type":"HTTP","url":"http://127.0.0.1:41234/mcp/ws-1","headers":{"Authorization":"Bearer secret-token"}},` +
	`{"name":"local","type":"stdio","command":"node","args":["server.js"],"env":{"TOKEN":"x"}}]}`

func TestSessionsAPI_MCPServersReachTheSpawnConfig(t *testing.T) {
	svc := newFakeSessionService()
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", mcpSpawnBody)
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	want := []ports.ChatMCPServerConfig{
		{Name: "coding-tools", Type: "http", URL: "http://127.0.0.1:41234/mcp/ws-1",
			Headers: map[string]string{"Authorization": "Bearer secret-token"}},
		{Name: "local", Type: "stdio", Command: "node", Args: []string{"server.js"}, Env: map[string]string{"TOKEN": "x"}},
	}
	if !reflect.DeepEqual(svc.lastSpawn.MCPServers, want) {
		t.Fatalf("spawn mcpServers=%#v\nwant %#v", svc.lastSpawn.MCPServers, want)
	}

	svc = newFakeSessionService()
	srv = newSessionTestServer(t, svc)
	if _, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/sessions",
		`{"kind":"worker","harness":"codex","mode":"chat","prompt":"hello"}`); status != http.StatusCreated || svc.lastSpawn.MCPServers != nil {
		t.Fatalf("a spawn without mcpServers: status=%d servers=%v", status, svc.lastSpawn.MCPServers)
	}
}

func TestSessionsAPI_MCPServersAreRefusedWhenInvalidOrNotChat(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		code string
	}{
		"tui mode": {strings.Replace(mcpSpawnBody, `"mode":"chat"`, `"mode":"tui"`, 1), "MCP_SERVERS_CHAT_ONLY"},
		"no mode":  {strings.Replace(mcpSpawnBody, `"mode":"chat",`, ``, 1), "MCP_SERVERS_CHAT_ONLY"},
		"bad url": {`{"harness":"codex","mode":"chat","mcpServers":[{"name":"x","type":"http","url":"file:///etc/passwd"}]}`,
			"MCP_SERVERS_INVALID"},
		"bad type": {`{"harness":"codex","mode":"chat","mcpServers":[{"name":"x","type":"websocket","url":"ws://127.0.0.1:1"}]}`,
			"MCP_SERVERS_INVALID"},
		"header injection": {`{"harness":"codex","mode":"chat","mcpServers":[{"name":"x","type":"http","url":"http://127.0.0.1:1","headers":{"Authorization":"Bearer secret-token\r\nX: y"}}]}`,
			"MCP_SERVERS_INVALID"},
	} {
		svc := newFakeSessionService()
		srv := newSessionTestServer(t, svc)
		body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", tc.body)
		if status != http.StatusBadRequest || !strings.Contains(string(body), tc.code) || svc.lastSpawn.Harness != "" {
			t.Fatalf("%s: status=%d spawned=%+v body=%s", name, status, svc.lastSpawn, body)
		}
		if strings.Contains(string(body), "secret-token") {
			t.Fatalf("%s: the refusal echoes a header value: %s", name, body)
		}
	}
}

func TestSessionsAPI_MCPServersAreRefusedFromTheLANListener(t *testing.T) {
	svc := newFakeSessionService()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Sessions: svc}, httpd.ControlDeps{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(controllers.WithLANRequest(r.Context())))
	}))
	t.Cleanup(srv.Close)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions", mcpSpawnBody)
	if status != http.StatusForbidden || !strings.Contains(string(body), "MCP_SERVERS_LOCAL_ONLY") || svc.lastSpawn.Harness != "" {
		t.Fatalf("LAN spawn with mcpServers: status=%d spawned=%+v body=%s", status, svc.lastSpawn, body)
	}
}
