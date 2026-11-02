package ports

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateChatMCPServersAcceptsEachTransport(t *testing.T) {
	servers := []ChatMCPServerConfig{
		{Name: "coding-tools", Type: "http", URL: "http://127.0.0.1:41234/mcp/ws-1",
			Headers: map[string]string{"Authorization": "Bearer abc"}},
		{Name: "remote_events", Type: "sse", URL: "https://example.test/sse"},
		{Name: "local", Type: "stdio", Command: "node", Args: []string{"server.js"}, Env: map[string]string{"TOKEN": "x"}},
	}
	if err := ValidateChatMCPServers(servers); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatMCPServers(nil); err != nil {
		t.Fatal(err)
	}
}

func TestValidateChatMCPServersRefusesMalformedEntriesWithoutEchoingValues(t *testing.T) {
	secret := "Bearer very-secret-token"
	tooMany := make([]ChatMCPServerConfig, MaxChatMCPServers+1)
	for index := range tooMany {
		tooMany[index] = ChatMCPServerConfig{Name: fmt.Sprintf("s%d", index), Type: "http", URL: "http://127.0.0.1:1"}
	}
	for name, servers := range map[string][]ChatMCPServerConfig{
		"too many":           tooMany,
		"no name":            {{Type: "http", URL: "http://127.0.0.1:1"}},
		"bad name":           {{Name: "a b", Type: "http", URL: "http://127.0.0.1:1"}},
		"duplicate name":     {{Name: "x", Type: "http", URL: "http://127.0.0.1:1"}, {Name: "X", Type: "http", URL: "http://127.0.0.1:2"}},
		"unknown type":       {{Name: "x", Type: "websocket", URL: "ws://127.0.0.1:1"}},
		"missing type":       {{Name: "x", URL: "http://127.0.0.1:1"}},
		"file url":           {{Name: "x", Type: "http", URL: "file:///etc/passwd"}},
		"relative url":       {{Name: "x", Type: "http", URL: "/mcp"}},
		"credentials in url": {{Name: "x", Type: "http", URL: "http://user:pass@127.0.0.1:1"}},
		"header newline":     {{Name: "x", Type: "http", URL: "http://127.0.0.1:1", Headers: map[string]string{"Authorization": secret + "\r\nX: y"}}},
		"bad header name":    {{Name: "x", Type: "http", URL: "http://127.0.0.1:1", Headers: map[string]string{"Bad Header": secret}}},
		"long header":        {{Name: "x", Type: "http", URL: "http://127.0.0.1:1", Headers: map[string]string{"Authorization": strings.Repeat("a", 9000)}}},
		"http with command":  {{Name: "x", Type: "http", URL: "http://127.0.0.1:1", Command: "node"}},
		"stdio no command":   {{Name: "x", Type: "stdio"}},
		"stdio with url":     {{Name: "x", Type: "stdio", Command: "node", URL: "http://127.0.0.1:1"}},
		"stdio bad env":      {{Name: "x", Type: "stdio", Command: "node", Env: map[string]string{"1BAD": secret}}},
		"stdio many args":    {{Name: "x", Type: "stdio", Command: "node", Args: make([]string, 65)}},
	} {
		err := ValidateChatMCPServers(servers)
		if !errors.Is(err, ErrInvalidMCPServers) {
			t.Fatalf("%s: err=%v, want ErrInvalidMCPServers", name, err)
		}
		if strings.Contains(err.Error(), "very-secret-token") || strings.Contains(err.Error(), "pass@") {
			t.Fatalf("%s: error echoes a value: %v", name, err)
		}
	}
}

func TestCloneChatMCPServersIsDeep(t *testing.T) {
	original := []ChatMCPServerConfig{{Name: "x", Type: "stdio", Command: "node", Args: []string{"a"},
		Env: map[string]string{"K": "v"}, Headers: map[string]string{"H": "v"}}}
	clone := CloneChatMCPServers(original)
	clone[0].Args[0], clone[0].Env["K"], clone[0].Headers["H"] = "changed", "changed", "changed"
	if original[0].Args[0] != "a" || original[0].Env["K"] != "v" || original[0].Headers["H"] != "v" {
		t.Fatalf("clone shares state with the original: %+v", original)
	}
	if CloneChatMCPServers(nil) != nil {
		t.Fatal("an empty list clones to nil")
	}
}
