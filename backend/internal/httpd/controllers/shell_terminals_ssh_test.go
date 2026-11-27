package controllers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

const (
	controllerSSHToken = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	sshTerminalPath    = "/api/v1/shell-terminals/ssh"
	sshTerminalBody    = `{"host":"build.example.com","port":22,"user":"deploy","identityFile":"C:\\keys\\id_ed25519","title":"Build server","client":{"major":9,"minor":5}}`
)

func newShellTerminalLANTestServer(t *testing.T, svc controllers.ShellTerminalService) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{ShellTerminals: svc}, httpd.ControlDeps{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(requestscope.WithLAN(r.Context())))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func doRequestWithHeaders(t *testing.T, srv *httptest.Server, method, path, body string, headers map[string]string) ([]byte, int) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// The SSH route requires a JSON content type, so every request carries one
	// unless a test sets another, or "" to send none.
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		if value == "" && name == "Content-Type" {
			req.Header.Del(name)
			continue
		}
		req.Header.Set(name, value)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf, resp.StatusCode
}

func tokenHeader(token string) map[string]string {
	return map[string]string{shelltermsvc.SSHTerminalTokenHeader: token}
}

type apiErrorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func expectAPIError(t *testing.T, body []byte, status, wantStatus int, wantCode, wantMessage string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", status, wantStatus, body)
	}
	var got apiErrorBody
	mustJSON(t, body, &got)
	if got.Code != wantCode {
		t.Fatalf("code = %q, want %q; body=%s", got.Code, wantCode, body)
	}
	if wantMessage != "" && got.Message != wantMessage {
		t.Fatalf("message = %q, want %q", got.Message, wantMessage)
	}
}

func TestShellTerminalsAPI_OpenSSHRefusesLANCallerFirst(t *testing.T) {
	for _, svc := range []*fakeShellTerminalService{{}, {sshToken: controllerSSHToken}} {
		srv := newShellTerminalLANTestServer(t, svc)
		for _, headers := range []map[string]string{nil, tokenHeader(controllerSSHToken)} {
			body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, "{not json", headers)
			expectAPIError(t, body, status, http.StatusForbidden, shelltermsvc.SSHTerminalLocalOnlyCode, "")
		}
		if svc.sshCalls != 0 {
			t.Fatal("a LAN request reached the service")
		}
	}
}

func TestShellTerminalsAPI_OpenSSHRouteOffBeforeTheCapability(t *testing.T) {
	svc := &fakeShellTerminalService{}
	srv := newShellTerminalTestServer(t, svc)
	for _, headers := range []map[string]string{nil, tokenHeader(controllerSSHToken)} {
		body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, "{not json", headers)
		expectAPIError(t, body, status, http.StatusNotFound, shelltermsvc.SSHTerminalDisabledCode, "")
	}
	if svc.sshCalls != 0 {
		t.Fatal("a disabled route reached the service")
	}
}

// A missing or wrong capability is refused before the body is read, with no
// detail about which.
func TestShellTerminalsAPI_OpenSSHNeedsTheCapability(t *testing.T) {
	svc := &fakeShellTerminalService{sshToken: controllerSSHToken}
	srv := newShellTerminalTestServer(t, svc)
	for _, headers := range []map[string]string{
		nil,
		tokenHeader(""),
		tokenHeader(controllerSSHToken[:63]),
		tokenHeader(strings.ToUpper(controllerSSHToken)),
		{"X-AO-SSH-Terminal-Token-Other": controllerSSHToken},
	} {
		for _, payload := range []string{sshTerminalBody, "{not json"} {
			body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, payload, headers)
			expectAPIError(t, body, status, http.StatusForbidden, shelltermsvc.SSHTerminalForbiddenCode, "This request is not allowed.")
			if strings.Contains(string(body), controllerSSHToken) {
				t.Fatal("the refusal echoes a token")
			}
		}
	}
	if svc.sshCalls != 0 {
		t.Fatal("a request without the capability reached the service")
	}
}

func TestShellTerminalsAPI_OpenSSHReadsTheBodyStrictly(t *testing.T) {
	const (
		object  = "Send the host, port, user and title as one JSON object."
		extra   = "SSH terminals take no extra options or arguments."
		invalid = "The request is not valid JSON."
		client  = "Client: send the OpenSSH version as two whole numbers from 0 to 999 (major and minor)."
	)
	for _, tc := range []struct {
		name, body, want string
	}{
		{"empty", "", "Send the host, port, user and title of the SSH terminal."},
		{"malformed", "{not json", invalid},
		{"truncated", `{"host":"build.example.com"`, invalid},
		{"array", `[]`, object},
		{"string", `"build.example.com"`, object},
		{"unknown field", `{"host":"build.example.com","user":"deploy","title":"t","options":["-oProxyCommand=calc"]}`, extra},
		{"case-variant key", `{"HOST":"build.example.com","user":"deploy","title":"t"}`, extra},
		{"two values", sshTerminalBody + ` {"host":"evil.example.com"}`, "Send one JSON object."},
		{"too large", `{"host":"build.example.com","title":"` + strings.Repeat("t", 17<<10) + `"}`, "The request is too large."},
		{"port as a string", `{"host":"build.example.com","port":"22","user":"deploy","title":"t"}`, "Port: send the port as a number from 1 to 65535. Leave it out for 22."},
		{"port with a fraction", `{"host":"build.example.com","port":22.5,"user":"deploy","title":"t"}`, "Port: use digits only, from 1 to 65535. Leave it empty for 22."},
		{"port as a boolean", `{"host":"build.example.com","port":true,"user":"deploy","title":"t"}`, "Port: use digits only, from 1 to 65535. Leave it empty for 22."},
		{"host as a number", `{"host":7,"user":"deploy","title":"t"}`, "Host: enter a host name or an IP address."},
		{"user as an object", `{"host":"build.example.com","user":{},"title":"t"}`, "User: enter the user name you sign in with on the server."},
		{"key as a number", `{"host":"build.example.com","user":"deploy","title":"t","identityFile":1}`, "Key file: choose the private key file by its full path."},
		{"title as a list", `{"host":"build.example.com","user":"deploy","title":["t"]}`, "Title: enter a name for this terminal."},
		{"client as a number", `{"host":"build.example.com","user":"deploy","title":"t","client":95}`, client},
		{"client extra key", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":9,"minor":5,"patch":2}}`, client},
		{"client fraction", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":9.5,"minor":0}}`, client},
		{"lone surrogate", `{"host":"build.example.com","user":"deploy","title":"t\ud800"}`, "The request is not valid text."},
		{"lone low surrogate", `{"host":"build.example.com","user":"deploy","title":"\udc00t"}`, "The request is not valid text."},
		{"invalid UTF-8", "{\"host\":\"build.example.com\",\"user\":\"deploy\",\"title\":\"t\xff\"}", "The request is not valid text."},
		{"top-level null", `null`, object},
		{"duplicate host", `{"host":"build.example.com","user":"deploy","title":"t","host":"evil.example.com"}`, "Host: send this field once."},
		{"duplicate client", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":9,"minor":5},"client":{"major":7,"minor":9}}`, "Client: send this field once."},
		{"duplicate key in client", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":9,"minor":5,"major":7}}`, "Client: send major and minor once each."},
		{"client null", `{"host":"build.example.com","user":"deploy","title":"t","client":null}`, client},
		{"client without minor", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":9}}`, client},
		{"client empty", `{"host":"build.example.com","user":"deploy","title":"t","client":{}}`, client},
		{"client major null", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":null,"minor":5}}`, client},
		{"client major as a list", `{"host":"build.example.com","user":"deploy","title":"t","client":{"major":[9],"minor":5}}`, client},
		{"host null", `{"host":null,"user":"deploy","title":"t"}`, "Host: enter a host name or an IP address."},
		{"user null", `{"host":"build.example.com","user":null,"title":"t"}`, "User: enter the user name you sign in with on the server."},
		{"title null", `{"host":"build.example.com","user":"deploy","title":null}`, "Title: enter a name for this terminal."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeShellTerminalService{sshToken: controllerSSHToken}
			srv := newShellTerminalTestServer(t, svc)
			body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, tc.body, tokenHeader(controllerSSHToken))
			expectAPIError(t, body, status, http.StatusBadRequest, shelltermsvc.SSHTerminalInvalidCode, tc.want)
			if svc.sshCalls != 0 {
				t.Fatal("a body that could not be read reached the service")
			}
		})
	}
}

func TestShellTerminalsAPI_OpenSSHPassesTypedFieldsWithAccess(t *testing.T) {
	opened := sampleShellTerminal()
	opened.HandleID = "shellterm-ssh-abc123"
	opened.Title = "Build server"
	svc := &fakeShellTerminalService{sshToken: controllerSSHToken, sshOpened: opened}
	srv := newShellTerminalTestServer(t, svc)

	body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, sshTerminalBody, tokenHeader(controllerSSHToken))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	var resp struct {
		ShellTerminal struct {
			HandleID string `json:"handleId"`
			Title    string `json:"title"`
		} `json:"shellTerminal"`
	}
	mustJSON(t, body, &resp)
	if resp.ShellTerminal.HandleID != "shellterm-ssh-abc123" || resp.ShellTerminal.Title != "Build server" {
		t.Fatalf("response = %+v", resp.ShellTerminal)
	}
	if !svc.gotSSHAccess {
		t.Fatal("the service was called without SSH terminal access")
	}
	major, minor, port := 9, 5, 22
	want := shelltermsvc.OpenSSHTerminalInput{
		Host: "build.example.com", Port: &port, User: "deploy", IdentityFile: `C:\keys\id_ed25519`, Title: "Build server",
		Client: &shelltermsvc.SSHClientVersionInput{Major: &major, Minor: &minor},
	}
	if !reflect.DeepEqual(svc.gotSSHInput, want) {
		t.Fatalf("service input = %#v, want %#v", svc.gotSSHInput, want)
	}
}

func TestShellTerminalsAPI_OpenSSHOptionalFields(t *testing.T) {
	svc := &fakeShellTerminalService{sshToken: controllerSSHToken, sshOpened: sampleShellTerminal()}
	srv := newShellTerminalTestServer(t, svc)
	body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, ` {"host":"build.example.com","port":null,"user":"deploy","title":"t","identityFile":null} `, tokenHeader(controllerSSHToken))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	if svc.gotSSHInput.Port != nil || svc.gotSSHInput.Client != nil || svc.gotSSHInput.IdentityFile != "" {
		t.Fatalf("optional fields = %#v, want them absent", svc.gotSSHInput)
	}
}

// The service's own refusals pass through in the error envelope.
func TestShellTerminalsAPI_OpenSSHServiceRefusals(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{apierr.Invalid(shelltermsvc.SSHTerminalInvalidCode, "Host: a host name cannot start with a hyphen.", nil), http.StatusBadRequest, shelltermsvc.SSHTerminalInvalidCode},
		{apierr.Conflict(shelltermsvc.SSHTerminalNoPinnedKeysCode, "x", nil), http.StatusConflict, shelltermsvc.SSHTerminalNoPinnedKeysCode},
		{apierr.Conflict(shelltermsvc.SSHTerminalNoClientCode, "x", nil), http.StatusConflict, shelltermsvc.SSHTerminalNoClientCode},
	} {
		svc := &fakeShellTerminalService{sshToken: controllerSSHToken, sshErr: tc.err}
		srv := newShellTerminalTestServer(t, svc)
		body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, sshTerminalBody, tokenHeader(controllerSSHToken))
		expectAPIError(t, body, status, tc.status, tc.code, "")
	}
}

// PATCH and DELETE carry SSH terminal access to the service only for a loopback
// request with this run's capability.
func TestShellTerminalsAPI_RenameAndCloseCarryAccessOnlyWithTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lan     bool
		headers map[string]string
		access  bool
	}{
		{"no header", false, nil, false},
		{"wrong token", false, tokenHeader(controllerSSHToken[:32]), false},
		{"capability", false, tokenHeader(controllerSSHToken), true},
		{"capability on the LAN listener", true, tokenHeader(controllerSSHToken), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renamed := sampleShellTerminal()
			svc := &fakeShellTerminalService{sshToken: controllerSSHToken, renamed: renamed}
			srv := newShellTerminalTestServer(t, svc)
			if tc.lan {
				srv = newShellTerminalLANTestServer(t, svc)
			}
			if body, status := doRequestWithHeaders(t, srv, "PATCH", "/api/v1/shell-terminals/shellterm-ssh-abc123", `{"title":"Build box"}`, tc.headers); status != http.StatusOK {
				t.Fatalf("rename status = %d; body=%s", status, body)
			}
			if body, status := doRequestWithHeaders(t, srv, "DELETE", "/api/v1/shell-terminals/shellterm-ssh-abc123", "", tc.headers); status != http.StatusNoContent {
				t.Fatalf("close status = %d; body=%s", status, body)
			}
			if svc.gotRenameAccess != tc.access || svc.gotCloseAccess != tc.access {
				t.Fatalf("access = rename %v close %v, want %v", svc.gotRenameAccess, svc.gotCloseAccess, tc.access)
			}
		})
	}
}

// The body must be sent as JSON; a utf-8 charset is the only parameter taken.
func TestShellTerminalsAPI_OpenSSHNeedsAJSONContentType(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		accepted    bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"Application/JSON; Charset=UTF-8", true},
		{"", false},
		{"text/plain", false},
		{"application/x-www-form-urlencoded", false},
		{"application/json; charset=latin1", false},
		{"application/json; boundary=x", false},
		{"application/json-patch+json", false},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			svc := &fakeShellTerminalService{sshToken: controllerSSHToken, sshOpened: sampleShellTerminal()}
			srv := newShellTerminalTestServer(t, svc)
			headers := tokenHeader(controllerSSHToken)
			headers["Content-Type"] = tc.contentType
			body, status := doRequestWithHeaders(t, srv, "POST", sshTerminalPath, sshTerminalBody, headers)
			if tc.accepted {
				if status != http.StatusCreated {
					t.Fatalf("status = %d, want 201; body=%s", status, body)
				}
				return
			}
			expectAPIError(t, body, status, http.StatusBadRequest, shelltermsvc.SSHTerminalInvalidCode, "Send the request as JSON (Content-Type: application/json).")
			if svc.sshCalls != 0 {
				t.Fatal("a request that is not JSON reached the service")
			}
		})
	}
}
