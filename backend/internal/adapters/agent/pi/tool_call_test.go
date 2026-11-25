package pi

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runFakeAOIfAsked makes the test binary stand in for `ao hooks pi tool-call`
// when the extension under test runs it: it keeps the tool call it was given
// and prints the answer the test chose.
func runFakeAOIfAsked() {
	answer, ok := os.LookupEnv("AO_TEST_FAKE_AO_ANSWER")
	if !ok || len(os.Args) < 4 || strings.Join(os.Args[1:4], " ") != "hooks pi tool-call" {
		return
	}
	stdin, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(os.Getenv("AO_TEST_FAKE_AO_STDIN"), stdin, 0o600)
	_, _ = os.Stdout.WriteString(answer)
	os.Exit(0)
}

// The tool_call handler puts each tool call but the read-only ones to the chat
// through `ao hooks pi tool-call`, only in a session AO set up for it, and
// blocks the tool on anything but an allow.
func TestPiExtensionAsksTheChatBeforeATool(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the Pi extension fixture")
	}
	fakeAO, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	modulePath := filepath.Join(dir, "ao-activity.mjs")
	source := strings.NewReplacer(
		`import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";`+"\n", "",
		`function callHookSync(hookName: string, payload: Record<string, unknown>)`, `function callHookSync(hookName, payload)`,
		`function sessionID(ctx: any): string`, `function sessionID(ctx)`,
		`export default function (pi: ExtensionAPI)`, `export default function (pi)`,
	).Replace(piActivityExtensionSource(true))
	if strings.Contains(source, `spawnSync(APPROVAL_AO`) {
		t.Fatal("the approval call must not block the pi-acp daemon")
	}
	if err := os.WriteFile(modulePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	harnessPath := filepath.Join(dir, "harness.mjs")
	if err := os.WriteFile(harnessPath, []byte(`import { pathToFileURL } from "node:url";
const handlers = {};
const loaded = await import(pathToFileURL(process.argv[2]).href);
loaded.default({ on(name, handler) { handlers[name] = handler; }, registerProvider() {} });
const result = await handlers.tool_call(JSON.parse(process.argv[3]), {});
process.stdout.write(JSON.stringify(result ?? null));
`), 0o600); err != nil {
		t.Fatal(err)
	}
	approval := []string{"AO_PI_APPROVAL_SESSION_ID=chat-1", "AO_PI_APPROVAL_TOKEN=secret", "AO_PI_APPROVAL_AO_BIN=" + fakeAO}
	bash := `{"type":"tool_call","toolCallId":"call-1","toolName":"bash","input":{"command":"rm -rf build"}}`
	for _, test := range []struct {
		name, event, answer string
		env                 []string
		want                string
		asked               bool
	}{
		{name: "a terminal session", event: bash, answer: `{"allow":false}`, want: "null"},
		{name: "a read", event: `{"toolCallId":"c","toolName":"read","input":{"path":"a.txt"}}`, answer: `{"allow":false}`, env: approval, want: "null"},
		{name: "allowed", event: bash, answer: `{"allow":true}` + "\n", env: approval, want: "null", asked: true},
		{name: "denied with the reviewer's reason", event: bash, answer: `{"allow":false,"reason":"The reviewer denied this"}` + "\n", env: approval,
			want: `{"block":true,"reason":"The reviewer denied this"}`, asked: true},
		{name: "denied", event: bash, answer: `{"allow":false}`, env: approval, want: `{"block":true,"reason":"Denied in the chat"}`, asked: true},
		{name: "no answer", event: bash, answer: "not json", env: approval, want: `{"block":true,"reason":"AO gave no answer about this tool call"}`, asked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdin := filepath.Join(t.TempDir(), "stdin.json")
			cmd := exec.Command(node, harnessPath, modulePath, test.event)
			cmd.Env = append(os.Environ(), "AO_PI_APPROVAL_SESSION_ID=", "AO_PI_APPROVAL_TOKEN=",
				"AO_TEST_FAKE_AO_ANSWER="+test.answer, "AO_TEST_FAKE_AO_STDIN="+stdin)
			cmd.Env = append(cmd.Env, test.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("extension fixture failed: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != test.want {
				t.Fatalf("tool_call result = %s, want %s", got, test.want)
			}
			raw, err := os.ReadFile(stdin)
			if asked := err == nil; asked != test.asked {
				t.Fatalf("asked AO = %v, want %v", asked, test.asked)
			}
			if test.asked {
				var call map[string]any
				if err := json.Unmarshal(raw, &call); err != nil || call["tool_name"] != "bash" || call["tool_call_id"] != "call-1" ||
					call["input"].(map[string]any)["command"] != "rm -rf build" {
					t.Fatalf("AO was given %s (%v)", raw, err)
				}
			}
		})
	}
}
