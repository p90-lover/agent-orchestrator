package httpd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

const (
	muxSSHToken = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	muxSSHID    = "shellterm-ssh-0011223344556677"
	muxPlainID  = "shellterm-0011223344556677"
)

// muxShellTerminals is the shell terminal service as the router sees it; only
// the SSH gate's two answers matter here.
type muxShellTerminals struct{}

func (muxShellTerminals) OpenShellTerminal(context.Context, shelltermsvc.OpenShellTerminalInput) (shelltermsvc.ShellTerminal, error) {
	return shelltermsvc.ShellTerminal{}, nil
}
func (muxShellTerminals) ListShellTerminalsForCurrentAppRun(context.Context) ([]shelltermsvc.ShellTerminal, error) {
	return nil, nil
}
func (muxShellTerminals) RenameShellTerminal(context.Context, string, string) (shelltermsvc.ShellTerminal, error) {
	return shelltermsvc.ShellTerminal{}, nil
}
func (muxShellTerminals) CloseShellTerminal(context.Context, string) error { return nil }
func (muxShellTerminals) OpenSSHTerminal(context.Context, shelltermsvc.OpenSSHTerminalInput) (shelltermsvc.ShellTerminal, error) {
	return shelltermsvc.ShellTerminal{}, nil
}
func (muxShellTerminals) SSHTerminalEnabled() bool { return true }
func (muxShellTerminals) SSHTerminalTokenMatches(presented string) bool {
	return presented == muxSSHToken
}
func (muxShellTerminals) IsProtectedTerminal(id string) bool { return id == muxSSHID }

// memoryStream is an attach Stream that stays open until closed.
type memoryStream struct {
	once   sync.Once
	closed chan struct{}
}

func (s *memoryStream) Read([]byte) (int, error) { <-s.closed; return 0, io.EOF }
func (s *memoryStream) Write(b []byte) (int, error) {
	return len(b), nil
}
func (s *memoryStream) Resize(uint16, uint16) error { return nil }
func (s *memoryStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// liveSource reports every terminal alive and attaches an in-memory stream.
type liveSource struct{}

func (liveSource) Attach(context.Context, ports.RuntimeHandle, uint16, uint16) (ports.Stream, error) {
	return &memoryStream{closed: make(chan struct{})}, nil
}
func (liveSource) IsAlive(context.Context, ports.RuntimeHandle) (bool, error) { return true, nil }

func newSSHMuxServer(t *testing.T, lan bool) string {
	t.Helper()
	mgr := terminal.NewManager(liveSource{}, nil, discardLogger(), terminal.WithHeartbeat(0))
	t.Cleanup(mgr.Close)
	router := NewRouterWithControl(config.Config{}, discardLogger(), mgr, APIDeps{ShellTerminals: muxShellTerminals{}}, ControlDeps{})
	var handler http.Handler = router
	if lan {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			router.ServeHTTP(w, r.WithContext(requestscope.WithLAN(r.Context())))
		})
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/mux"
}

// openOnMux opens id on a fresh connection and answers the first of opened or
// exited it is sent.
func openOnMux(t *testing.T, url string, header http.Header, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial /mux: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")
	if err := wsjson.Write(ctx, c, terminalMuxFrame{Ch: "terminal", ID: id, Type: "open"}); err != nil {
		t.Fatalf("write open: %v", err)
	}
	for {
		var f terminalMuxFrame
		if err := wsjson.Read(ctx, c, &f); err != nil {
			t.Fatalf("waiting for opened or exited: %v", err)
		}
		if f.Ch == "terminal" && f.ID == id && (f.Type == "opened" || f.Type == "exited") {
			return f.Type
		}
	}
}

// The capability is read from the upgrade: with it the SSH terminal opens;
// without it, with a wrong one, or on the LAN listener it fails as an unknown
// terminal does. Other terminals open either way.
func TestMuxUpgradeGatesSSHTerminalsOnTheCapability(t *testing.T) {
	withToken := http.Header{shelltermsvc.SSHTerminalTokenHeader: []string{muxSSHToken}}
	wrongToken := http.Header{shelltermsvc.SSHTerminalTokenHeader: []string{muxSSHToken[:32]}}
	for _, tc := range []struct {
		name   string
		lan    bool
		header http.Header
		ssh    string
	}{
		{"capability", false, withToken, "opened"},
		{"no header", false, nil, "exited"},
		{"wrong token", false, wrongToken, "exited"},
		{"capability on the LAN listener", true, withToken, "exited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newSSHMuxServer(t, tc.lan)
			if got := openOnMux(t, url, tc.header, muxSSHID); got != tc.ssh {
				t.Fatalf("SSH terminal open = %s, want %s", got, tc.ssh)
			}
			if got := openOnMux(t, url, tc.header, muxPlainID); got != "opened" {
				t.Fatalf("plain terminal open = %s, want opened", got)
			}
		})
	}
}
