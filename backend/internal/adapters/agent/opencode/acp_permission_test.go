package opencode

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// openCodeRule and the helpers below replay how OpenCode 1.19.5 decides a
// permission (packages/opencode/src/permission/index.ts fromConfig and
// evaluate, util/wildcard.ts match, agent/agent.ts): a custom agent's ruleset
// is OpenCode's defaults, then the config's `permission`, then the agent's own
// `permission`, each object read in key order, and the last matching rule wins.
type openCodeRule struct{ permission, pattern, action string }

func openCodeRules(t *testing.T, raw string) []openCodeRule {
	t.Helper()
	rules, err := orderedPermissionRules(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("rules %s: %v", raw, err)
	}
	var out []openCodeRule
	for _, key := range rules.keys {
		value := rules.values[key]
		var action string
		if json.Unmarshal(value, &action) == nil {
			out = append(out, openCodeRule{key, "*", action})
			continue
		}
		patterns, err := orderedPermissionRules(value)
		if err != nil {
			t.Fatalf("patterns %s: %v", value, err)
		}
		for _, pattern := range patterns.keys {
			if err := json.Unmarshal(patterns.values[pattern], &action); err != nil {
				t.Fatalf("action %s: %v", patterns.values[pattern], err)
			}
			out = append(out, openCodeRule{key, pattern, action})
		}
	}
	return out
}

func openCodeWildcard(str, pattern string) bool {
	escaped := regexp.QuoteMeta(pattern)
	escaped = strings.ReplaceAll(escaped, `\*`, ".*")
	escaped = strings.ReplaceAll(escaped, `\?`, ".")
	if strings.HasSuffix(escaped, " .*") {
		escaped = strings.TrimSuffix(escaped, " .*") + "( .*)?"
	}
	return regexp.MustCompile("(?s)^" + escaped + "$").MatchString(str)
}

func openCodeEvaluate(rules []openCodeRule, permission, pattern string) string {
	for i := len(rules) - 1; i >= 0; i-- {
		if openCodeWildcard(permission, rules[i].permission) && openCodeWildcard(pattern, rules[i].pattern) {
			return rules[i].action
		}
	}
	return "ask"
}

// openCodeDefaults is the start of OpenCode 1.19.5's default ruleset, the part
// these cases touch.
const openCodeDefaults = `{"*":"allow","doom_loop":"ask","external_directory":{"*":"ask"},"question":"deny","read":{"*":"allow","*.env":"ask"}}`

func TestAskingTiersAskWhereOpenCodeWouldAllowAndLeaveTheUsersRulesDeciding(t *testing.T) {
	for _, test := range []struct {
		name string
		user string
		mode ports.PermissionMode
		// permission, pattern, action OpenCode decides on the AO agent
		want [][3]string
	}{
		{
			name: "no rules of the user's own",
			mode: ports.PermissionModeAcceptEdits,
			want: [][3]string{
				{"bash", "git status", "ask"}, {"webfetch", "https://example.com", "ask"},
				{"websearch", "*", "ask"}, {"task", "general", "ask"},
				{"edit", "a.txt", "allow"}, {"read", "a.txt", "allow"}, {"read", "x.env", "ask"},
				{"question", "*", "deny"}, {"external_directory", "/tmp/x", "ask"},
			},
		},
		{
			name: "default asks before an edit",
			mode: ports.PermissionModeDefault,
			want: [][3]string{{"edit", "a.txt", "ask"}, {"bash", "ls", "ask"}, {"read", "a.txt", "allow"}},
		},
		{
			name: "a denied command stays denied and an allowed one runs",
			user: `{"bash":{"git *":"allow","rm *":"deny"}}`,
			mode: ports.PermissionModeAcceptEdits,
			want: [][3]string{{"bash", "rm -rf build", "deny"}, {"bash", "git status", "allow"}, {"bash", "npm test", "ask"}},
		},
		{
			name: "a blanket deny keeps its later exceptions",
			// The user's own order: deny everything, then allow the shell. One
			// shared "bash" key would have put AO's ask and the user's allow
			// ahead of the user's "*", denying the shell.
			user: `{"*":"deny","bash":"allow","read":"allow"}`,
			mode: ports.PermissionModeAcceptEdits,
			want: [][3]string{{"bash", "ls", "allow"}, {"webfetch", "https://example.com", "deny"}, {"read", "a.txt", "allow"}},
		},
		{
			name: "the user's own catch-all decides",
			user: `"allow"`,
			mode: ports.PermissionModeDefault,
			want: [][3]string{{"bash", "ls", "allow"}, {"edit", "a.txt", "allow"}},
		},
		{
			name: "a webfetch deny survives",
			user: `{"webfetch":"deny","edit":"ask"}`,
			mode: ports.PermissionModeAcceptEdits,
			want: [][3]string{{"webfetch", "https://example.com", "deny"}, {"edit", "a.txt", "ask"}, {"bash", "ls", "ask"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent, err := acpAskingTierPermission(test.mode, json.RawMessage(test.user))
			if err != nil {
				t.Fatalf("acpAskingTierPermission: %v", err)
			}
			if !json.Valid(agent) {
				t.Fatalf("ruleset is not JSON: %s", agent)
			}
			user := test.user
			if user == "" {
				user = "{}"
			}
			var rules []openCodeRule
			for _, layer := range []string{openCodeDefaults, user, string(agent)} {
				rules = append(rules, openCodeRules(t, layer)...)
			}
			for _, want := range test.want {
				if got := openCodeEvaluate(rules, want[0], want[1]); got != want[2] {
					t.Errorf("%s %q = %s, want %s (agent ruleset %s)", want[0], want[1], got, want[2], agent)
				}
			}
		})
	}
}

func TestAskingTierRulesetKeepsTheUsersKeyOrder(t *testing.T) {
	user := `{"*":"deny","bash":{"git *":"allow","rm *":"deny"},"read":"allow","bash*":"allow"}`
	agent, err := acpAskingTierPermission(ports.PermissionModeAuto, json.RawMessage(user))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := orderedPermissionRules(agent)
	if err != nil {
		t.Fatal(err)
	}
	// The user's own "bash*" replaces AO's, in the user's place.
	want := []string{"webfetch*", "websearch*", "task*", "*", "bash", "read", "bash*"}
	if strings.Join(rules.keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v (%s)", rules.keys, want, agent)
	}
	if got := string(rules.values["bash"]); got != `{"git *":"allow","rm *":"deny"}` {
		t.Fatalf("bash patterns = %s, want the user's in their order", got)
	}
}

func TestCheckPermissionConfigRefusesWhatItCannotRestate(t *testing.T) {
	for _, raw := range []string{"", "null", `"ask"`, `{}`, `{"bash":{"rm *":"deny"}}`} {
		if err := CheckPermissionConfig(json.RawMessage(raw)); err != nil {
			t.Errorf("CheckPermissionConfig(%s) = %v", raw, err)
		}
	}
	for _, raw := range []string{`[]`, `42`, `{"bash":`, `{"a":1}x`} {
		if err := CheckPermissionConfig(json.RawMessage(raw)); err == nil {
			t.Errorf("CheckPermissionConfig(%s) accepted it", raw)
		}
	}
}

func TestPrepareACPConfigContentRestatesTheUsersRulesOnEveryAskingTier(t *testing.T) {
	content, err := PrepareACPConfigContent("", "rules", ports.PermissionModeDefault,
		json.RawMessage(`{"bash":{"rm *":"deny"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Agent map[string]struct {
			Permission json.RawMessage `json:"permission"`
		} `json:"agent"`
	}
	if err := json.Unmarshal([]byte(content), &config); err != nil {
		t.Fatal(err)
	}
	for _, tier := range []string{"ao-default", "ao-accept-edits", "ao-auto"} {
		if got := string(config.Agent[tier].Permission); !strings.HasSuffix(got, `"bash":{"rm *":"deny"}}`) {
			t.Errorf("%s permission = %s, want the user's rules last", tier, got)
		}
	}
	if got := string(config.Agent["ao-bypass"].Permission); got != `"allow"` {
		t.Errorf("bypass permission = %s", got)
	}
}
