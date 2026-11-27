package httpd

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// terminalMuxReadLimit caps a single inbound frame. Client→server frames are small
// (keystrokes, resize, control), so a generous 1 MiB is ample headroom while
// still bounding memory per message.
const terminalMuxReadLimit = 1 << 20

// sshTerminalMuxAccess is what the mux needs from the shell terminal service to
// gate the in-app SSH terminal: which terminals are protected, and whether an
// upgrade presented the capability.
type sshTerminalMuxAccess interface {
	terminal.ProtectedTerminals
	SSHTerminalTokenMatches(presented string) bool
}

// mountTerminalMux registers the long-lived terminal-multiplexing WebSocket at /mux. It
// is intentionally outside the per-request Timeout middleware (the connection is
// long-lived). When mgr is nil the route is not mounted — the daemon simply has
// no terminal surface yet.
//
// The shell terminal service, when wired, is bound to the manager here as its
// protected-terminal authority, so the mux and the REST routes always agree on
// which terminals are SSH terminals.
func mountTerminalMux(r chi.Router, mgr *terminal.Manager, shellTerminals sshTerminalMuxAccess, log *slog.Logger) {
	if mgr == nil {
		return
	}
	if shellTerminals != nil {
		mgr.SetProtectedTerminals(shellTerminals)
	}
	r.Get("/mux", terminalMuxHandler(mgr, shellTerminals, log))
}

// terminalMuxHandler upgrades the request to a WebSocket and hands the connection to the
// terminal manager. httpd owns only the upgrade and the transport adaptation;
// all stream logic lives in internal/terminal.
func terminalMuxHandler(mgr *terminal.Manager, access sshTerminalMuxAccess, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Decided once, at the upgrade, from the socket the request arrived on
		// and the header the app's gateway sets; a browser page cannot set a
		// header on a WebSocket upgrade.
		var opts []terminal.ServeOption
		if muxMayOpenSSHTerminals(r, access) {
			opts = append(opts, terminal.WithProtectedAccess())
		}
		// InsecureSkipVerify disables coder/websocket's same-origin check: the
		// daemon binds loopback only and the desktop renderer's origin differs
		// from the loopback host, mirroring the legacy Node mux server.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			log.Warn("terminal mux: websocket upgrade failed", "err", err)
			return
		}
		c.SetReadLimit(terminalMuxReadLimit)
		mgr.Serve(r.Context(), &terminalMuxConn{c: c}, opts...)
	}
}

// muxMayOpenSSHTerminals reports whether this upgrade may open SSH terminals:
// it arrived on the loopback listener and carries this run's capability. A
// missing or wrong token is not refused; the connection simply cannot open an
// SSH terminal, and every other terminal behaves as before.
func muxMayOpenSSHTerminals(r *http.Request, access sshTerminalMuxAccess) bool {
	if access == nil || requestscope.IsLAN(r.Context()) {
		return false
	}
	return access.SSHTerminalTokenMatches(r.Header.Get(shelltermsvc.SSHTerminalTokenHeader))
}

// terminalMuxConn adapts a coder/websocket connection to terminal.wsConn. JSON framing
// uses wsjson (text messages); Ping is a control frame; Close sends a normal
// closure.
type terminalMuxConn struct{ c *websocket.Conn }

func (a *terminalMuxConn) ReadJSON(ctx context.Context, v any) error { return wsjson.Read(ctx, a.c, v) }
func (a *terminalMuxConn) WriteJSON(ctx context.Context, v any) error {
	return wsjson.Write(ctx, a.c, v)
}
func (a *terminalMuxConn) Ping(ctx context.Context) error { return a.c.Ping(ctx) }
func (a *terminalMuxConn) Close(reason string) error {
	return a.c.Close(websocket.StatusNormalClosure, reason)
}
