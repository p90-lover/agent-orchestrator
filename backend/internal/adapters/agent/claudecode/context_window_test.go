package claudecode

import (
	"testing"
)

func TestContextWindowEnvPreservesCompactionAndUserSettings(t *testing.T) {
	env := map[string]string{"DISABLE_COMPACT": "0", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50", "KEEP": "user"}
	if err := setContextWindowEnv("gpt-5.5(high)", 32768, "2.1.286 (Claude Code)", env); err != nil {
		t.Fatal(err)
	}
	if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "32768" || env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "32768" || env["DISABLE_COMPACT"] != "0" ||
		env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] != "50" || env["KEEP"] != "user" {
		t.Fatalf("context changed unrelated settings: %+v", env)
	}
}

func TestContextWindowEnvRejectsUnknownVersion(t *testing.T) {
	for _, version := range []string{"", "not-a-version", "2.1.192", "1.99.999"} {
		if err := setContextWindowEnv("gpt-5.5", 32768, version, map[string]string{}); err == nil {
			t.Errorf("accepted unverified version %q", version)
		}
	}
}

// Claude models took no window at all, so the composer's picker was off for every one of them. A
// recognized model (claude-…, an alias) compacts at AUTO_COMPACT_WINDOW, a CPA alias declares its
// window with MAX_CONTEXT_TOKENS; both are set for every model.
func TestContextWindowEnvCoversClaudeModelsAndAliases(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-5-5[1m]", "Claude Opus 5.5", "opus", "sonnet(high)", "default", ""} {
		env := map[string]string{}
		if err := setContextWindowEnv(model, 150000, "2.1.310 (Claude Code)", env); err != nil {
			t.Fatalf("%q: %v", model, err)
		}
		if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "150000" || env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "150000" {
			t.Errorf("%q window env=%+v", model, env)
		}
	}
}
