package controllers_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
)

const wantAgentFileCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads"

func assertAgentFileHeaders(t *testing.T, what string, headers http.Header) {
	t.Helper()
	if got := headers.Get("Content-Security-Policy"); got != wantAgentFileCSP {
		t.Fatalf("%s: Content-Security-Policy = %q, want %q", what, got, wantAgentFileCSP)
	}
	if strings.Contains(headers.Get("Content-Security-Policy"), "allow-same-origin") {
		t.Fatalf("%s: the sandbox keeps the same origin", what)
	}
	if got := headers.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("%s: X-Content-Type-Options = %q, want nosniff", what, got)
	}
}

// The legacy preview route serves agent-written HTML on the daemon's origin:
// every response is sandboxed into an opaque origin and never sniffed.
func TestSessionsAPI_LegacyPreviewFilesAreSandboxed(t *testing.T) {
	svc := newFakeSessionService()
	workspace := t.TempDir()
	files := map[string]string{
		"page.html":  `<script>new WebSocket("ws://" + location.host + "/mux")</script>`,
		"notes.md":   "# Notes\n\n<script>alert(1)</script>\n",
		"script.js":  `fetch("/api/v1/shell-terminals")`,
		"report.htm": `<h1>report</h1>`,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := svc.sessions["ao-1"]
	s.Metadata = domain.SessionMetadata{WorkspacePath: workspace}
	svc.sessions["ao-1"] = s
	srv := newSessionTestServer(t, svc)

	for _, tc := range []struct {
		path     string
		wantType string
	}{
		{"page.html", "text/html"},
		{"report.htm", "text/html"},
		{"notes.md", "text/html"},
		{"script.js", "javascript"},
	} {
		body, status, headers := doRequest(t, srv, http.MethodGet, "/api/v1/sessions/ao-1/preview/files/"+tc.path, "")
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d; body=%s", tc.path, status, body)
		}
		if !strings.Contains(headers.Get("Content-Type"), tc.wantType) {
			t.Fatalf("GET %s Content-Type = %q, want %s", tc.path, headers.Get("Content-Type"), tc.wantType)
		}
		assertAgentFileHeaders(t, "GET "+tc.path, headers)
	}
	// A missing file is answered under the same headers.
	_, status, headers := doRequest(t, srv, http.MethodGet, "/api/v1/sessions/ao-1/preview/files/missing.html", "")
	if status != http.StatusNotFound {
		t.Fatalf("missing file = %d, want 404", status)
	}
	assertAgentFileHeaders(t, "missing file", headers)
}

// An attachment the app staged is served through the same route and headers.
func TestSessionsAPI_LegacyPreviewAttachmentIsSandboxed(t *testing.T) {
	svc := newFakeSessionService()
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".ao", "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".ao", "attachments", "attachment-a.png"), []byte("\x89PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := svc.sessions["ao-1"]
	s.Metadata = domain.SessionMetadata{WorkspacePath: workspace}
	svc.sessions["ao-1"] = s
	srv := newSessionTestServer(t, svc)
	body, status, headers := doRequest(t, srv, http.MethodGet, "/api/v1/sessions/ao-1/preview/files/.ao/attachments/attachment-a.png", "")
	if status != http.StatusOK {
		t.Fatalf("attachment = %d; body=%s", status, body)
	}
	assertAgentFileHeaders(t, "attachment", headers)
}

func TestSessionsAPI_WorkspaceFileBlobIsSandboxed(t *testing.T) {
	svc := newFakeSessionService()
	svc.workspaceBlob = sessionsvc.WorkspaceFileBlob{Path: "docs/logo.png", MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}}
	srv := newSessionTestServer(t, svc)
	body, status, headers := doRequest(t, srv, http.MethodGet, "/api/v1/sessions/ao-1/workspace/file/blob?path=docs%2Flogo.png", "")
	if status != http.StatusOK {
		t.Fatalf("blob = %d; body=%s", status, body)
	}
	assertAgentFileHeaders(t, "workspace blob", headers)
}

// The isolated preview origin is its own origin, and an app built for it
// (module scripts, storage) needs that origin: it is left as it was.
func TestSessionsAPI_PreviewOriginKeepsItsOwnOrigin(t *testing.T) {
	svc := newFakeSessionService()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "index.html"), []byte(`<script type="module" src="/app.js"></script>`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := svc.sessions["ao-1"]
	s.Metadata = domain.SessionMetadata{WorkspacePath: workspace}
	svc.sessions["ao-1"] = s
	srv := newSessionTestServer(t, svc)
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/sessions/ao-1/preview", `{"url":"./index.html"}`)
	if status != http.StatusOK {
		t.Fatalf("set preview = %d; body=%s", status, body)
	}
	var resp struct {
		Session struct {
			PreviewURL string `json:"previewUrl"`
		} `json:"session"`
	}
	mustJSON(t, body, &resp)
	_, status, headers := doPreviewOriginRequest(t, srv, resp.Session.PreviewURL, "/")
	if status != http.StatusOK {
		t.Fatalf("preview origin = %d", status)
	}
	if headers.Get("Content-Security-Policy") != "" {
		t.Fatalf("preview origin CSP = %q, want none", headers.Get("Content-Security-Policy"))
	}
}
