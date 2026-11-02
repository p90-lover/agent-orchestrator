package codexappserver

import (
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// addMCPServerConfig carries AO-supplied tool servers into a thread/start or
// thread/resume config override. Each server is one dotted key,
// "mcp_servers.<name>", so it is added next to the servers in the user's own
// config.toml instead of replacing that table. Codex speaks stdio and
// streamable HTTP; an SSE server is refused rather than silently dropped.
//
// Header values (an Authorization bearer, typically) travel as Codex's static
// http_headers and reach only the local app-server process.
func addMCPServerConfig(config map[string]any, servers []ports.ChatMCPServerConfig) error {
	for _, server := range servers {
		name := strings.TrimSpace(server.Name)
		if name == "" || strings.ContainsAny(name, ". \"'") {
			return fmt.Errorf("codex MCP server name %q is not a plain identifier", server.Name)
		}
		entry := map[string]any{}
		switch strings.ToLower(strings.TrimSpace(server.Type)) {
		case "", ports.ChatMCPTransportStdio:
			if strings.TrimSpace(server.Command) == "" {
				return fmt.Errorf("codex MCP server %q requires a command", name)
			}
			entry["command"] = server.Command
			if len(server.Args) > 0 {
				entry["args"] = append([]string(nil), server.Args...)
			}
			if len(server.Env) > 0 {
				entry["env"] = copyStrings(server.Env)
			}
		case ports.ChatMCPTransportHTTP:
			if strings.TrimSpace(server.URL) == "" {
				return fmt.Errorf("codex MCP server %q requires a URL", name)
			}
			entry["url"] = server.URL
			if len(server.Headers) > 0 {
				entry["http_headers"] = copyStrings(server.Headers)
			}
		case ports.ChatMCPTransportSSE:
			return fmt.Errorf("codex does not support SSE MCP server %q; use http", name)
		default:
			return fmt.Errorf("unsupported codex MCP transport %q for %q", server.Type, name)
		}
		config["mcp_servers."+name] = entry
	}
	return nil
}

func copyStrings(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
