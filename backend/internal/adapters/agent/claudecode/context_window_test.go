package claudecode

import (
	"testing"
)

func TestContextWindowEnvPreservesCompactionAndUserSettings(t *testing.T) {
	env := map[string]string{"DISABLE_COMPACT": "0", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50", "KEEP": "user"}
	if err := setContextWindowEnv("gpt-5.5(high)", 32768, "2.1.286 (Claude Code)", env); err != nil {
		t.Fatal(err)
	}
	if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "32768" || env["DISABLE_COMPACT"] != "0" ||
		env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] != "50" || env["KEEP"] != "user" {
		t.Fatalf("context changed unrelated settings: %+v", env)
	}
}

func TestContextWindowEnvRejectsUnknownVersionAndRecognizedModels(t *testing.T) {
	for _, version := range []string{"", "not-a-version", "2.1.192", "1.99.999"} {
		if err := setContextWindowEnv("gpt-5.5", 32768, version, map[string]string{}); err == nil {
			t.Errorf("accepted unverified version %q", version)
		}
	}
	env := map[string]string{}
	if err := setContextWindowEnv("claude-sonnet-4-5", 32768, "2.1.286", env); err == nil || len(env) != 0 {
		t.Fatalf("recognized Claude model override=%+v, %v", env, err)
	}
}
