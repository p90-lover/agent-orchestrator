package controllers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type lanRequestKey struct{}

// WithLANRequest marks a request that arrived on the Connect Mobile LAN
// listener. The mark comes from the socket the request used, which a client
// cannot spoof, unlike the Host header.
func WithLANRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, lanRequestKey{}, true)
}

// IsLANRequest reports whether WithLANRequest marked ctx.
func IsLANRequest(ctx context.Context) bool {
	marked, _ := ctx.Value(lanRequestKey{}).(bool)
	return marked
}

type spawnMCPError struct {
	status  int
	kind    string
	code    string
	message string
}

// spawnMCPServers converts and validates a spawn's tool servers. A stdio
// server is a command the daemon runs, so the list is taken from loopback
// callers only; a paired phone cannot add one. It applies to an explicitly
// requested chat session, the only mode with a controller to attach it to.
func spawnMCPServers(r *http.Request, in SpawnSessionRequest) ([]ports.ChatMCPServerConfig, *spawnMCPError) {
	if len(in.MCPServers) == 0 {
		return nil, nil
	}
	if IsLANRequest(r.Context()) {
		return nil, &spawnMCPError{http.StatusForbidden, "forbidden", "MCP_SERVERS_LOCAL_ONLY",
			"mcpServers can only be supplied by a local client"}
	}
	if in.Mode != domain.SessionModeChat {
		return nil, &spawnMCPError{http.StatusBadRequest, "validation", "MCP_SERVERS_CHAT_ONLY",
			"mcpServers require mode \"chat\""}
	}
	servers := make([]ports.ChatMCPServerConfig, 0, len(in.MCPServers))
	for _, server := range in.MCPServers {
		servers = append(servers, ports.ChatMCPServerConfig{
			Name: server.Name, Type: strings.ToLower(strings.TrimSpace(server.Type)),
			Command: server.Command, Args: server.Args, Env: server.Env,
			URL: server.URL, Headers: server.Headers,
		})
	}
	if err := ports.ValidateChatMCPServers(servers); err != nil {
		message := err.Error()
		if !errors.Is(err, ports.ErrInvalidMCPServers) {
			message = "mcpServers are invalid"
		}
		return nil, &spawnMCPError{http.StatusBadRequest, "validation", "MCP_SERVERS_INVALID", message}
	}
	return servers, nil
}
