package codexappserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RTK (github.com/rtk-ai/rtk) rewrites the agent's shell commands to compact
// equivalents ("git status" -> "rtk git status"), so the model reads a fraction
// of their output. Claude Code chats get RTK's hook in their own session
// settings (claudeacp); this is the same for Codex's own shell, through
// Codex's PreToolUse hooks, registered on the app-server's command line only:
// never in the person's config.toml or hooks.json.

// EnvRTKCodexHook carries RTK's Codex hook command from Coding Tools, which
// sets it while RTK is installed and turned on and leaves it empty otherwise.
// It is read from the app-server's own environment, where the session's value
// overrides the daemon's: a session's empty value turns it off.
const EnvRTKCodexHook = "CODING_TOOLS_RTK_CODEX_HOOK"

// rtkHookMatcher selects Codex's shell tools: shell commands and unified exec
// (exec_command) both match as Bash, and write_stdin never runs PreToolUse.
const rtkHookMatcher = "^Bash$"

// rtkHookTimeoutSec bounds one hook run. Codex runs the command as the model
// wrote it when a hook fails or times out, so a stalled rtk costs at most this
// long per command instead of Codex's 600 s default.
const rtkHookTimeoutSec = 10

// codexRTKHook is the hook command in an app-server launch environment
// (KEY=VALUE, as processenv.Merge builds it), or "" for none or for one that
// is not a single plain command line. Codex runs it with the turn's own shell
// (PowerShell on Windows), so Coding Tools passes `rtk hook codex` and keeps
// rtk's folder first on the agents' PATH: the command rtk returns calls `rtk`
// too, and without rtk on PATH the hook fails and the command runs unchanged.
func codexRTKHook(env []string) string {
	hook := ""
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && (key == EnvRTKCodexHook || (runtime.GOOS == "windows" && strings.EqualFold(key, EnvRTKCodexHook))) {
			hook = value
		}
	}
	hook = strings.TrimSpace(hook)
	// U+2028 and U+2029 are refused with the control characters: Go's JSON
	// encoder escapes them where Codex's does not, so the trust hash would
	// differ and Codex would skip the hook as modified.
	if hook == "" || len(hook) > 1024 || !utf8.ValidString(hook) || strings.ContainsFunc(hook, func(r rune) bool {
		return r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029'
	}) {
		return ""
	}
	return hook
}

// appServerArgs is app-server's argv after the Codex binary. With RTK's hook
// it starts with two config overrides, which Codex loads as its session-flags
// layer: the PreToolUse hook itself, and this process's trust in exactly that
// hook. Codex skips any non-managed hook nobody has trusted, and a trust
// passed in thread/start's config is not read (both checked against Codex
// 0.164.0); trust passed here covers this one hook and no other.
func appServerArgs(env []string) []string {
	hook := codexRTKHook(env)
	if hook == "" {
		return []string{"app-server"}
	}
	return []string{
		"-c", "hooks.PreToolUse=[{matcher=" + tomlString(rtkHookMatcher) +
			`,hooks=[{type="command",command=` + tomlString(hook) +
			",timeout=" + strconv.Itoa(rtkHookTimeoutSec) + "}]}]",
		"-c", "hooks.state={" + tomlString(sessionFlagsHookKey()) +
			"={trusted_hash=" + tomlString(codexHookTrustHash(hook, rtkHookTimeoutSec)) + "}}",
		"app-server",
	}
}

// sessionFlagsHookKey is Codex's key for the first handler of the first
// PreToolUse group in its session-flags layer: the layer's synthetic path,
// rooted at C:\ on Windows and / elsewhere, then event, group and handler.
func sessionFlagsHookKey() string {
	if runtime.GOOS == "windows" {
		return `C:\<session-flags>\config.toml:pre_tool_use:0:0`
	}
	return "/<session-flags>/config.toml:pre_tool_use:0:0"
}

// codexHookTrustHash is the hash Codex trusts a PreToolUse command hook by:
// sha256 over the compact, key-sorted JSON of the hook's normalized identity
// (codex-rs hooks/src/engine/discovery.rs hook_hash and config/src/
// fingerprint.rs version_for_toml). Codex normalizes a command handler to its
// type, command, timeout and async flag; the options AO never sets are absent.
func codexHookTrustHash(command string, timeoutSec int) string {
	identity := map[string]any{
		"event_name": "pre_tool_use",
		"matcher":    rtkHookMatcher,
		"hooks": []any{map[string]any{
			"async": false, "command": command, "timeout": timeoutSec, "type": "command",
		}},
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// serde_json leaves <, > and & as they are; Go would write \u003c and so on.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(identity); err != nil {
		return ""
	}
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tomlString quotes s as a TOML basic string. codexRTKHook has already
// refused the control characters that TOML would need escape sequences for.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
