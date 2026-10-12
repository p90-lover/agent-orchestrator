package ports

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Caller-supplied tool servers for a chat session (POST /api/v1/sessions
// mcpServers). They are additive to the agent's own configuration and are
// handed only to the local provider process. Validation errors name the server
// and the field, never a value: env and header values routinely carry tokens.

// ErrInvalidMCPServers reports a malformed caller-supplied tool server list.
var ErrInvalidMCPServers = errors.New("invalid mcpServers")

const (
	// MaxChatMCPServers bounds how many tool servers one session may add.
	MaxChatMCPServers = 8
	// MCP transports a chat session accepts.
	ChatMCPTransportStdio = "stdio"
	ChatMCPTransportHTTP  = "http"
	ChatMCPTransportSSE   = "sse"

	maxMCPCommandBytes = 1024
	maxMCPArgs         = 64
	maxMCPArgBytes     = 4096
	maxMCPEnvEntries   = 64
	maxMCPHeaders      = 32
	maxMCPValueBytes   = 8192
	maxMCPURLBytes     = 2048
)

var (
	mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	mcpEnvNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	mcpHeaderNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,128}$")
)

// ValidateChatMCPServers checks a caller-supplied tool server list: at most
// MaxChatMCPServers entries with unique names; type stdio, http or sse; a
// stdio server has a command and no URL or headers; an http/sse server has an
// absolute http(s) URL without credentials and no command, args or env; and
// every count and length is bounded.
func ValidateChatMCPServers(servers []ChatMCPServerConfig) error {
	if len(servers) > MaxChatMCPServers {
		return fmt.Errorf("%w: at most %d servers are allowed", ErrInvalidMCPServers, MaxChatMCPServers)
	}
	seen := make(map[string]struct{}, len(servers))
	for index, server := range servers {
		if !mcpServerNamePattern.MatchString(server.Name) {
			return fmt.Errorf("%w: server %d needs a name of 1-64 letters, digits, '_' or '-'", ErrInvalidMCPServers, index)
		}
		key := strings.ToLower(server.Name)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: server name %q is used twice", ErrInvalidMCPServers, server.Name)
		}
		seen[key] = struct{}{}
		if err := validateChatMCPServer(server); err != nil {
			return fmt.Errorf("%w: server %q: %s", ErrInvalidMCPServers, server.Name, err.Error())
		}
	}
	return nil
}

func validateChatMCPServer(server ChatMCPServerConfig) error {
	switch server.Type {
	case ChatMCPTransportStdio:
		if strings.TrimSpace(server.Command) == "" || len(server.Command) > maxMCPCommandBytes || hasControl(server.Command) {
			return errors.New("command is required and must be a single line of at most 1024 bytes")
		}
		if len(server.Args) > maxMCPArgs {
			return fmt.Errorf("at most %d args are allowed", maxMCPArgs)
		}
		for index, arg := range server.Args {
			if len(arg) > maxMCPArgBytes || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("arg %d is too long or contains NUL", index)
			}
		}
		if len(server.Env) > maxMCPEnvEntries {
			return fmt.Errorf("at most %d env entries are allowed", maxMCPEnvEntries)
		}
		for name, value := range server.Env {
			if !mcpEnvNamePattern.MatchString(name) {
				return errors.New("env names must be identifiers of at most 128 characters")
			}
			if len(value) > maxMCPValueBytes || strings.ContainsRune(value, 0) {
				return fmt.Errorf("env %s value is too long or contains NUL", name)
			}
		}
		if server.URL != "" || len(server.Headers) > 0 {
			return errors.New("a stdio server takes no url or headers")
		}
	case ChatMCPTransportHTTP, ChatMCPTransportSSE:
		if err := validateMCPServerURL(server.URL); err != nil {
			return err
		}
		if len(server.Headers) > maxMCPHeaders {
			return fmt.Errorf("at most %d headers are allowed", maxMCPHeaders)
		}
		for name, value := range server.Headers {
			if !mcpHeaderNamePattern.MatchString(name) {
				return errors.New("header names must be HTTP tokens of at most 128 characters")
			}
			if len(value) > maxMCPValueBytes || hasControl(value) {
				return fmt.Errorf("header %s value is too long or contains control characters", name)
			}
		}
		if server.Command != "" || len(server.Args) > 0 || len(server.Env) > 0 {
			return fmt.Errorf("an %s server takes no command, args or env", server.Type)
		}
	default:
		return errors.New("type must be stdio, http or sse")
	}
	return nil
}

func validateMCPServerURL(raw string) error {
	if raw == "" || len(raw) > maxMCPURLBytes || hasControl(raw) {
		return errors.New("url is required and must be at most 2048 bytes")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" {
		return errors.New("url must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return errors.New("url must not carry credentials; use headers")
	}
	return nil
}

func hasControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// CloneChatMCPServers deep-copies a server list so a retained launch contract
// cannot be changed through the caller's maps and slices.
func CloneChatMCPServers(servers []ChatMCPServerConfig) []ChatMCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	out := make([]ChatMCPServerConfig, len(servers))
	for index, server := range servers {
		server.Args = append([]string(nil), server.Args...)
		if server.Env != nil {
			env := make(map[string]string, len(server.Env))
			for key, value := range server.Env {
				env[key] = value
			}
			server.Env = env
		}
		if server.Headers != nil {
			headers := make(map[string]string, len(server.Headers))
			for key, value := range server.Headers {
				headers[key] = value
			}
			server.Headers = headers
		}
		out[index] = server
	}
	return out
}
