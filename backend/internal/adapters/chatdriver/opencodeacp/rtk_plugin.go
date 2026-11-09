package opencodeacp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// RTK (github.com/rtk-ai/rtk) rewrites the agent's shell commands to compact
// equivalents ("git status" -> "rtk git status"), so the model reads a fraction
// of their output. Claude Code chats get RTK's hook in their own session
// settings (claudeacp) and Codex chats on their app-server's command line
// (codexappserver); this is the same for OpenCode's own bash tool, through an
// OpenCode plugin that Coding Tools writes and AO lists in the session's inline
// overlay only: never in the person's OpenCode config. OpenCode appends the
// overlay's plugins to the ones the person configured (checked against
// OpenCode 1.19.5), so theirs keep loading.

// EnvRTKOpenCodePlugin carries the absolute path of that plugin file from
// Coding Tools, which sets it while RTK is installed and turned on and leaves
// it empty otherwise. It is read from the session's environment, then the
// daemon's: a session's empty value turns it off.
const EnvRTKOpenCodePlugin = "CODING_TOOLS_RTK_OPENCODE_PLUGIN"

// rtkOpenCodePlugin is the plugin's file:// URL, or "" for none, for a value
// that is not an absolute path, or for a file that is not there. OpenCode runs
// a chat without a plugin it cannot load, so a missing file only costs RTK;
// it is still left out, so the chat's overlay never names a file that is gone.
func rtkOpenCodePlugin(env map[string]string) string {
	value, ok := env[EnvRTKOpenCodePlugin]
	if !ok {
		value = os.Getenv(EnvRTKOpenCodePlugin)
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) ||
		!filepath.IsAbs(value) {
		return ""
	}
	value = filepath.Clean(value)
	if info, err := os.Stat(value); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return fileURL(value)
}

// fileURL is path as a file:// URL OpenCode imports: forward slashes, a
// leading slash before a Windows drive letter, and spaces and other reserved
// characters percent-encoded ("G:\AppData\Coding Tools\rtk.ts" ->
// "file:///G:/AppData/Coding%20Tools/rtk.ts").
func fileURL(path string) string {
	slashed := filepath.ToSlash(path)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed}).String()
}

// withRTKPlugin adds RTK's OpenCode plugin to the session's inline overlay
// when Coding Tools passed one. Plugins already listed stay first and in
// order; a "plugin" value that is not a list is left for OpenCode to report,
// without RTK.
func withRTKPlugin(content string, env map[string]string) (string, error) {
	plugin := rtkOpenCodePlugin(env)
	if plugin == "" {
		return content, nil
	}
	config := map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(content))
	// Numbers the caller wrote stay as written (a float64 round trip would
	// turn a 1000000-token limit into 1e+06).
	decoder.UseNumber()
	if strings.TrimSpace(content) != "" {
		if err := decoder.Decode(&config); err != nil {
			return "", fmt.Errorf("opencode: decode OPENCODE_CONFIG_CONTENT: %w", err)
		}
	}
	var plugins []any
	switch listed := config["plugin"].(type) {
	case nil:
	case []any:
		plugins = listed
	default:
		return content, nil
	}
	for _, entry := range plugins {
		if entry == plugin {
			return content, nil
		}
	}
	config["plugin"] = append(plugins, plugin)
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(config); err != nil {
		return "", fmt.Errorf("opencode: encode ACP config with RTK plugin: %w", err)
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}
