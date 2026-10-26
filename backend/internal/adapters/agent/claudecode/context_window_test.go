package claudecode

import (
	"context"
	"maps"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/processenv"
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

func TestContextWindowEnvRejectsUnknownOrUnsupportedVersions(t *testing.T) {
	for _, version := range []string{"", "not-a-version", "2.1.192", "1.99.999"} {
		if err := setContextWindowEnv("gpt-5.5", 32768, version, map[string]string{}); err == nil {
			t.Errorf("accepted unverified version %q", version)
		}
	}
	for _, version := range []string{"2.1.193", "2.1.286", "2.1.303"} {
		env := map[string]string{"KEEP": "user"}
		if err := setContextWindowEnv("claude-opus-5-5", 128000, version, env); err == nil || !strings.Contains(err.Error(), "2.1.304") || len(env) != 1 {
			t.Errorf("native budget accepted unverified version %q: env=%+v, err=%v", version, env, err)
		}
	}
}

func TestContextWindowEnvNativeModelsKeepCompactionAndSettings(t *testing.T) {
	t.Setenv("DISABLE_AUTO_COMPACT", "0")
	for _, model := range []string{"", "default", "best", "opusplan", "opus", "sonnet(high)", "claude-opus-5-5", "anthropic/claude-sonnet-4-5", "custom[1m]"} {
		env := map[string]string{"DISABLE_COMPACT": "0", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "50", "KEEP": "user"}
		if err := setContextWindowEnv(model, 128000, "2.1.304 (Claude Code)", env); err != nil {
			t.Errorf("model %q: %v", model, err)
			continue
		}
		if env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "128000" || env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "" ||
			env["DISABLE_COMPACT"] != "0" || env["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"] != "50" || env["KEEP"] != "user" {
			t.Errorf("model %q changed unrelated settings or lost budget: %+v", model, env)
		}
	}
}

func TestContextWindowEnvModelSwitchMasksInheritedTuning(t *testing.T) {
	t.Setenv("DISABLE_COMPACT", "0")
	t.Setenv("DISABLE_AUTO_COMPACT", "0")
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "100000")
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "999999")
	env := map[string]string{"claude_code_auto_compact_window": "stale", "claude_code_max_context_tokens": "stale", "KEEP": "user"}
	for _, test := range []struct {
		model, version, active, inactive string
		tokens                           int64
	}{
		{"gpt-5.5", "2.1.193", "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", 32768},
		{"claude-opus-5-5", "2.1.310", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_CODE_MAX_CONTEXT_TOKENS", 200000},
		{"my-gateway-model", "2.1.310", "CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_CODE_AUTO_COMPACT_WINDOW", 128000},
	} {
		if err := setContextWindowEnv(test.model, test.tokens, test.version, env); err != nil {
			t.Fatal(err)
		}
		if value, present := env[test.inactive]; !present || value != "" {
			t.Fatalf("%s left inherited tuning active: %+v", test.model, env)
		}
		if env[test.active] != strconv.FormatInt(test.tokens, 10) {
			t.Fatalf("%s lost the selected context budget: %+v", test.model, env)
		}
		for _, entry := range processenv.Merge(env) {
			if strings.HasPrefix(entry, test.inactive+"=") && entry != test.inactive+"=" {
				t.Fatalf("%s inherited a competing context value: %s", test.model, entry)
			}
		}
		if _, present := env["claude_code_auto_compact_window"]; present {
			t.Fatal("case-insensitive stale context override survived")
		}
		if _, present := env["claude_code_max_context_tokens"]; present {
			t.Fatal("case-insensitive stale context override survived")
		}
		if env["KEEP"] != "user" {
			t.Fatal("unrelated user environment changed")
		}
	}
}

func TestContextWindowEnvRejectsDisabledAutoCompactionWithoutMutation(t *testing.T) {
	for _, flag := range []string{"DISABLE_COMPACT", "DISABLE_AUTO_COMPACT"} {
		for _, source := range []string{"supplied", "inherited"} {
			for index, value := range []string{"1", "TrUe", "YeS", "oN", " \ttrue\n"} {
				t.Run(flag+"/"+source+"/"+strconv.Itoa(index), func(t *testing.T) {
					t.Setenv("DISABLE_COMPACT", "0")
					t.Setenv("DISABLE_AUTO_COMPACT", "0")
					env := map[string]string{"KEEP": "user", "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "32768", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "200000"}
					if source == "supplied" {
						env[flag] = value
					} else {
						t.Setenv(flag, value)
					}
					before := maps.Clone(env)
					err := setContextWindowEnv("opus", 128000, "2.1.310", env)
					if err == nil || !strings.Contains(err.Error(), flag) || !maps.Equal(env, before) {
						t.Fatalf("disabled budget err=%v, env=%+v; want refusal without mutation", err, env)
					}
				})
			}
		}
	}
}

func TestContextWindowEnvCompactionOverlayFollowsLaunchPrecedence(t *testing.T) {
	for _, flag := range []string{"DISABLE_COMPACT", "DISABLE_AUTO_COMPACT"} {
		for _, value := range []string{"0", "FaLsE", "nO", "oFf", "", "2", "t"} {
			t.Run(flag+"/false/"+value, func(t *testing.T) {
				t.Setenv("DISABLE_COMPACT", "0")
				t.Setenv("DISABLE_AUTO_COMPACT", "0")
				t.Setenv(flag, "yes")
				env := map[string]string{flag: value}
				if err := setContextWindowEnv("opus", 128000, "2.1.310", env); err != nil || env[flag] != value {
					t.Fatalf("explicit false override %q: err=%v, env=%+v", value, err, env)
				}
			})
		}
		for _, test := range []struct {
			name, inherited string
			env             map[string]string
			reject          bool
		}{
			{"lowercase-disables", "0", map[string]string{strings.ToLower(flag): "on"}, runtime.GOOS == "windows"},
			{"lowercase-overrides-parent", "1", map[string]string{strings.ToLower(flag): "0"}, runtime.GOOS != "windows"},
			{"duplicate-spellings", "0", map[string]string{flag: "1", strings.ToLower(flag): "0"}, runtime.GOOS != "windows"},
		} {
			t.Run(flag+"/"+test.name, func(t *testing.T) {
				t.Setenv("DISABLE_COMPACT", "0")
				t.Setenv("DISABLE_AUTO_COMPACT", "0")
				t.Setenv(flag, test.inherited)
				before := maps.Clone(test.env)
				err := setContextWindowEnv("opus", 128000, "2.1.310", test.env)
				if (err != nil) != test.reject || (test.reject && !maps.Equal(test.env, before)) {
					t.Fatalf("case-sensitive merge err=%v, env=%+v, want refusal=%v", err, test.env, test.reject)
				}
			})
		}
	}
}

func TestContextWindowEnvDisabledCompactionKeepsAutoAndCustomBehavior(t *testing.T) {
	t.Setenv("DISABLE_COMPACT", "1")
	t.Setenv("DISABLE_AUTO_COMPACT", "1")
	env := map[string]string{"DISABLE_COMPACT": "yes", "DISABLE_AUTO_COMPACT": "true", "KEEP": "user"}
	before := maps.Clone(env)
	if err := ApplyContextWindowEnv(context.Background(), "missing-claude-binary", "opus", 0, env); err != nil || !maps.Equal(env, before) {
		t.Fatalf("Auto changed disabled compaction: err=%v, env=%+v", err, env)
	}
	if err := setContextWindowEnv("my-gateway-model", 32768, "2.1.193", env); err != nil ||
		env["DISABLE_COMPACT"] != "yes" || env["DISABLE_AUTO_COMPACT"] != "true" || env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "32768" {
		t.Fatalf("custom context behavior changed: err=%v, env=%+v", err, env)
	}
}

func TestContextWindowEnvAutoAndInvalidRequestsDoNotMutateEnvironment(t *testing.T) {
	env := map[string]string{"KEEP": "user"}
	if err := ApplyContextWindowEnv(context.Background(), "missing-claude-binary", "opus", 0, env); err != nil || len(env) != 1 {
		t.Fatalf("Auto changed settings or probed the binary: env=%+v, err=%v", env, err)
	}
	for _, tokens := range []int64{-1, 99999, 1000001} {
		if err := setContextWindowEnv("opus", tokens, "2.1.310", env); err == nil || len(env) != 1 {
			t.Errorf("invalid tokens %d changed settings: env=%+v, err=%v", tokens, env, err)
		}
	}
	if err := setContextWindowEnv("opus", 128000, "2.1.310", nil); err == nil {
		t.Fatal("explicit budget accepted a missing launch environment")
	}
}
