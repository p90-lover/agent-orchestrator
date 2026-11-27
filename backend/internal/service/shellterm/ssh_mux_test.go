package shellterm

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// muxFrame is the wire shape of one terminal mux frame, both directions.
type muxFrame struct {
	Ch   string `json:"ch"`
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

// jsonConn is an in-memory mux connection that speaks JSON, as the WebSocket
// adapter does, so the manager's own frame types never leave its package.
type jsonConn struct {
	in     chan muxFrame
	out    chan muxFrame
	once   sync.Once
	closed chan struct{}
}

func newJSONConn() *jsonConn {
	return &jsonConn{in: make(chan muxFrame, 16), out: make(chan muxFrame, 64), closed: make(chan struct{})}
}

func (c *jsonConn) ReadJSON(ctx context.Context, v any) error {
	select {
	case frame := <-c.in:
		raw, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, v)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return io.EOF
	}
}

func (c *jsonConn) WriteJSON(_ context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var frame muxFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return err
	}
	c.out <- frame
	return nil
}

func (c *jsonConn) Ping(context.Context) error { return nil }

func (c *jsonConn) Close(string) error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// openResult opens id and answers the first of opened or exited.
func (c *jsonConn) openResult(t *testing.T, id string) string {
	t.Helper()
	c.in <- muxFrame{Ch: "terminal", ID: id, Type: "open", Rows: 24, Cols: 80}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case frame := <-c.out:
			if frame.Ch == "terminal" && frame.ID == id && (frame.Type == "opened" || frame.Type == "exited") {
				return frame.Type
			}
		case <-deadline:
			t.Fatalf("no opened or exited frame for %s", id)
		}
	}
}

type idleStream struct {
	once   sync.Once
	closed chan struct{}
}

func (s *idleStream) Read([]byte) (int, error)    { <-s.closed; return 0, io.EOF }
func (s *idleStream) Write(b []byte) (int, error) { return len(b), nil }
func (s *idleStream) Resize(uint16, uint16) error { return nil }
func (s *idleStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// runtimeSource answers liveness from the fake runtime the service opened
// terminals in, and attaches an idle stream.
type runtimeSource struct{ rt *fakeShellRuntime }

func (s runtimeSource) IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	return s.rt.IsAlive(ctx, handle)
}

func (s runtimeSource) Attach(context.Context, ports.RuntimeHandle, uint16, uint16) (ports.Stream, error) {
	return &idleStream{closed: make(chan struct{})}, nil
}

// The /mux gate with the real service as its authority: a connection with
// protected access opens an SSH terminal, a tokenless one gets what an
// unknown terminal gets, and every other terminal is unchanged.
func TestSSHTerminalMuxGate(t *testing.T) {
	for _, prefix := range []string{"", "x-", "ptyhost-v1:"} {
		t.Run("runtime prefix "+prefix, func(t *testing.T) {
			rig := newSSHTestRig(t)
			rig.rt.handlePrefix = prefix
			sshTerminal, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
			if err != nil {
				t.Fatalf("OpenSSHTerminal: %v", err)
			}
			plain, err := rig.svc.OpenCommandTerminal(context.Background(), OpenCommandTerminalInput{Argv: []string{"pi"}, Title: "Log in to Pi"})
			if err != nil {
				t.Fatalf("OpenCommandTerminal: %v", err)
			}

			rig.rt.aliveByHandle["shellterm-ssh-7"] = true

			mgr := terminal.NewManager(runtimeSource{rt: rig.rt}, nil, testLogger(), terminal.WithHeartbeat(0))
			defer mgr.Close()
			mgr.SetProtectedTerminals(rig.svc)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serve := func(opts ...terminal.ServeOption) *jsonConn {
				conn := newJSONConn()
				go mgr.Serve(ctx, conn, opts...)
				return conn
			}

			app := serve(terminal.WithProtectedAccess())
			if got := app.openResult(t, sshTerminal.HandleID); got != "opened" {
				t.Fatalf("app open of the SSH terminal = %s, want opened", got)
			}
			if got := app.openResult(t, plain.HandleID); got != "opened" {
				t.Fatalf("app open of a plain terminal = %s, want opened", got)
			}
			tokenless := serve()
			if got := tokenless.openResult(t, sshTerminal.HandleID); got != "exited" {
				t.Fatalf("tokenless open of the SSH terminal = %s, want exited", got)
			}
			if got := tokenless.openResult(t, "shellterm-never-existed"); got != "exited" {
				t.Fatalf("tokenless open of an unknown terminal = %s, want exited", got)
			}
			if got := tokenless.openResult(t, plain.HandleID); got != "opened" {
				t.Fatalf("tokenless open of a plain terminal = %s, want opened", got)
			}
			// An agent terminal of a project whose session prefix is
			// "shellterm-ssh" is not of the minted shape and stays reachable.
			if got := tokenless.openResult(t, "shellterm-ssh-7"); got != "opened" {
				t.Fatalf("tokenless open of a prefixed agent terminal = %s, want opened", got)
			}
		})
	}
}
