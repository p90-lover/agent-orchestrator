package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPiToolCallHookAsksTheChatAndPrintsItsAnswer(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   piToolCallAnswer
	}{
		{name: "allowed", status: http.StatusOK, body: `{"allow":true}`, want: piToolCallAnswer{Allow: true}},
		{name: "denied", status: http.StatusOK, body: `{"allow":false}`, want: piToolCallAnswer{Reason: piToolApprovalDeniedText}},
		{name: "denied by the reviewer", status: http.StatusOK, body: `{"allow":false,"message":"The reviewer denied this"}`,
			want: piToolCallAnswer{Reason: "The reviewer denied this"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(envPiApprovalSessionID, "chat-1")
			t.Setenv(envPiApprovalToken, "secret")
			cfg := setConfigEnv(t)
			var got struct {
				path, auth string
				body       map[string]any
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/tool-approval") { // the CLI's own telemetry
					w.WriteHeader(http.StatusNoContent)
					return
				}
				got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
				_ = json.NewDecoder(r.Body).Decode(&got.body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer srv.Close()
			writeRunFileFor(t, cfg, srv)
			stdout, _, err := executeCLI(t, Deps{
				In:           strings.NewReader(`{"tool_call_id":"call-1","tool_name":"bash","input":{"command":"rm -rf build"}}`),
				ProcessAlive: func(int) bool { return true },
			}, "hooks", "pi", "tool-call")
			if err != nil {
				t.Fatal(err)
			}
			var answer piToolCallAnswer
			if err := json.Unmarshal([]byte(stdout), &answer); err != nil || answer != test.want {
				t.Fatalf("answer = %q (%v), want %+v", stdout, err, test.want)
			}
			if got.path != "/internal/chat/sessions/chat-1/tool-approval" || got.auth != "Bearer secret" {
				t.Fatalf("request = %s with %q", got.path, got.auth)
			}
			if got.body["kind"] != "execute" || got.body["title"] != "rm -rf build" || got.body["toolName"] != "bash" ||
				got.body["toolCallId"] != "call-1" {
				t.Fatalf("body = %#v", got.body)
			}
		})
	}
}

// Anything but an allow from the chat is a deny: the extension blocks the tool.
func TestPiToolCallHookDeniesWhenItCannotAsk(t *testing.T) {
	t.Run("no approval env", func(t *testing.T) {
		t.Setenv(envPiApprovalSessionID, "")
		t.Setenv(envPiApprovalToken, "")
		stdout, _, err := executeCLI(t, Deps{In: strings.NewReader(`{"tool_name":"bash"}`)}, "hooks", "pi", "tool-call")
		if err != nil || !strings.Contains(stdout, `"allow":false`) || !strings.Contains(stdout, "no chat approval settings") {
			t.Fatalf("stdout = %q, err = %v", stdout, err)
		}
	})
	t.Run("the daemon refuses", func(t *testing.T) {
		t.Setenv(envPiApprovalSessionID, "chat-1")
		t.Setenv(envPiApprovalToken, "wrong")
		cfg := setConfigEnv(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status":"forbidden"}`)
		}))
		defer srv.Close()
		writeRunFileFor(t, cfg, srv)
		stdout, _, err := executeCLI(t, Deps{
			In: strings.NewReader(`{"tool_name":"bash","input":{}}`), ProcessAlive: func(int) bool { return true },
		}, "hooks", "pi", "tool-call")
		if err != nil || !strings.Contains(stdout, `"allow":false`) || !strings.Contains(stdout, "AO could not ask the chat") {
			t.Fatalf("stdout = %q, err = %v", stdout, err)
		}
	})
}

func TestPiToolApprovalSubject(t *testing.T) {
	for _, test := range []struct {
		call        string
		kind, title string
	}{
		{`{"tool_name":"bash","input":{"command":"go test ./..."}}`, "execute", "go test ./..."},
		{`{"tool_name":"edit","input":{"path":"src/a.go","oldText":"x","newText":"y"}}`, "edit", "src/a.go"},
		{`{"tool_name":"write","input":{"path":"b.md","content":"hi"}}`, "edit", "b.md"},
		{`{"tool_name":"web_search","input":{"query":"q"}}`, "other", "web_search"},
		{`{"tool_name":"bash","input":{"command":"` + strings.Repeat("x", 300) + `"}}`, "execute", strings.Repeat("x", 200) + "…"},
	} {
		var call piToolCallPayload
		if err := json.Unmarshal([]byte(test.call), &call); err != nil {
			t.Fatal(err)
		}
		if kind, title := piToolApprovalSubject(call); kind != test.kind || title != test.title {
			t.Errorf("%s: (%q, %q), want (%q, %q)", test.call, kind, title, test.kind, test.title)
		}
	}
}
