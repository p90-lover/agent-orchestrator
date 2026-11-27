package terminal

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// protectedSet is a ProtectedTerminals fake.
type protectedSet struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (p *protectedSet) IsProtectedTerminal(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ids[id]
}

func (p *protectedSet) protect(id string) {
	p.mu.Lock()
	p.ids[id] = true
	p.mu.Unlock()
}

// perIDSource is a Source whose terminals are alive or gone per id, each with
// its own fake PTY, and which counts the runtime calls made for every id.
type perIDSource struct {
	mu      sync.Mutex
	alive   map[string]bool
	ptys    map[string]*fakePTY
	probes  map[string]int
	attachs map[string]int
}

func newPerIDSource(alive ...string) *perIDSource {
	s := &perIDSource{alive: map[string]bool{}, ptys: map[string]*fakePTY{}, probes: map[string]int{}, attachs: map[string]int{}}
	for _, id := range alive {
		s.alive[id] = true
		s.ptys[id] = newFakePTY()
	}
	return s
}

func (s *perIDSource) IsAlive(_ context.Context, handle ports.RuntimeHandle) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes[handle.ID]++
	return s.alive[handle.ID], nil
}

func (s *perIDSource) Attach(_ context.Context, handle ports.RuntimeHandle, _, _ uint16) (ports.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachs[handle.ID]++
	pty := s.ptys[handle.ID]
	if pty == nil {
		pty = newFakePTY()
	}
	return pty, nil
}

func (s *perIDSource) calls(id string) (probes, attaches int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes[id], s.attachs[id]
}

func serveTestConn(t *testing.T, mgr *Manager, opts ...ServeOption) *fakeConn {
	t.Helper()
	conn := newFakeConn()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go mgr.Serve(ctx, conn, opts...)
	return conn
}

// terminalFrames collects the terminal-channel frames for id until an exited
// frame arrives or quiet passes with nothing new.
func terminalFrames(t *testing.T, c *fakeConn, id string, quiet time.Duration) []serverMsg {
	t.Helper()
	var frames []serverMsg
	for {
		select {
		case m := <-c.out:
			if m.Ch != chTerminal || m.ID != id {
				continue
			}
			frames = append(frames, m)
			if m.Type == msgExited {
				return frames
			}
		case <-time.After(quiet):
			return frames
		}
	}
}

func frameTypes(frames []serverMsg) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Type)
	}
	return out
}

const (
	sshID   = "shellterm-ssh-1"
	plainID = "shellterm-plain-1"
	ghostID = "shellterm-ghost-1"
)

func newProtectedTestManager(src Source) (*Manager, *protectedSet) {
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	set := &protectedSet{ids: map[string]bool{sshID: true}}
	mgr.SetProtectedTerminals(set)
	return mgr, set
}

// A connection without protected access that opens an SSH terminal gets the
// frames an open of an unknown terminal gets, and the runtime is never asked
// about it.
func TestServeTokenlessOpenOfProtectedTerminalFailsAsUnknown(t *testing.T) {
	src := newPerIDSource(sshID)
	mgr, _ := newProtectedTestManager(src)
	defer mgr.Close()

	ghost := serveTestConn(t, mgr)
	ghost.in <- clientMsg{Ch: chTerminal, ID: ghostID, Type: msgOpen, Rows: 24, Cols: 80}
	wantFrames := terminalFrames(t, ghost, ghostID, time.Second)

	tokenless := serveTestConn(t, mgr)
	tokenless.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgOpen, Rows: 24, Cols: 80}
	gotFrames := terminalFrames(t, tokenless, sshID, time.Second)

	if got, want := frameTypes(gotFrames), frameTypes(wantFrames); !equalStrings(got, want) {
		t.Fatalf("frames for a protected terminal = %v, want the unknown terminal's %v", got, want)
	}
	if len(gotFrames) == 0 || gotFrames[len(gotFrames)-1].Type != msgExited {
		t.Fatalf("frames = %v, want them to end with exited", frameTypes(gotFrames))
	}
	for i := range gotFrames {
		if gotFrames[i].Cols != wantFrames[i].Cols || gotFrames[i].Rows != wantFrames[i].Rows {
			t.Fatalf("frame %d = %+v, want the unknown terminal's %+v", i, gotFrames[i], wantFrames[i])
		}
	}
	if probes, attaches := src.calls(sshID); probes != 0 || attaches != 0 {
		t.Fatalf("runtime calls for the protected terminal = %d probes, %d attaches; want none", probes, attaches)
	}
}

// The app's connection (protected access) opens and types into the SSH
// terminal; a tokenless connection that opens it alongside cannot type into
// it, resize it, or learn its grid.
func TestServeProtectedAccessOpensProtectedTerminal(t *testing.T) {
	src := newPerIDSource(sshID)
	mgr, _ := newProtectedTestManager(src)
	defer mgr.Close()

	app := serveTestConn(t, mgr, WithProtectedAccess())
	app.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgOpen, Rows: 24, Cols: 80}
	recv(t, app, chTerminal, msgOpened, time.Second)
	pty := src.ptys[sshID]
	pty.push([]byte("remote$ "))
	data := recv(t, app, chTerminal, msgData, time.Second)
	if got, _ := base64.StdEncoding.DecodeString(data.Data); string(got) != "remote$ " {
		t.Fatalf("streamed data = %q", got)
	}
	app.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("ls\r"))}
	eventually(t, time.Second, func() bool { return string(pty.writtenBytes()) == "ls\r" })
	resizesBefore := len(pty.resizeCalls())

	tokenless := serveTestConn(t, mgr)
	tokenless.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgOpen, Rows: 60, Cols: 200}
	tokenless.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("rm -rf ~\r"))}
	tokenless.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgResize, Rows: 70, Cols: 250, Force: true}
	frames := terminalFrames(t, tokenless, sshID, 300*time.Millisecond)
	for _, f := range frames {
		if f.Type == msgOpened || f.Type == msgData {
			t.Fatalf("tokenless connection got %s for the protected terminal", f.Type)
		}
		if f.Type == msgResize && (f.Cols == 80 || f.Rows == 24) {
			t.Fatalf("tokenless connection learnt the protected terminal's grid: %+v", f)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := string(pty.writtenBytes()); got != "ls\r" {
		t.Fatalf("pty input = %q, want only the app's input", got)
	}
	if got := len(pty.resizeCalls()); got != resizesBefore {
		t.Fatalf("pty resized %d more times by a tokenless connection", got-resizesBefore)
	}
	for {
		select {
		case m := <-app.out:
			if m.Type == msgResize && (m.Cols == 200 || m.Cols == 250) {
				t.Fatalf("the app's connection got a resize caused by a tokenless connection: %+v", m)
			}
			continue
		default:
		}
		break
	}
}

// Terminals that are not protected behave as before on a tokenless
// connection, and as before on a connection with protected access.
func TestServeUnprotectedTerminalUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []ServeOption
	}{
		{"tokenless", nil},
		{"protected access", []ServeOption{WithProtectedAccess()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newPerIDSource(plainID)
			mgr, _ := newProtectedTestManager(src)
			defer mgr.Close()

			conn := serveTestConn(t, mgr, tc.opts...)
			conn.in <- clientMsg{Ch: chTerminal, ID: plainID, Type: msgOpen}
			recv(t, conn, chTerminal, msgOpened, time.Second)
			pty := src.ptys[plainID]
			pty.push([]byte("prompt$ "))
			recv(t, conn, chTerminal, msgData, time.Second)
			conn.in <- clientMsg{Ch: chTerminal, ID: plainID, Type: msgData, Data: base64.StdEncoding.EncodeToString([]byte("whoami\n"))}
			eventually(t, time.Second, func() bool { return string(pty.writtenBytes()) == "whoami\n" })
			conn.in <- clientMsg{Ch: chTerminal, ID: plainID, Type: msgResize, Rows: 30, Cols: 100}
			eventually(t, time.Second, func() bool {
				rs := pty.resizeCalls()
				return len(rs) > 0 && rs[len(rs)-1] == [2]uint16{30, 100}
			})
		})
	}
}

// A manager with no protected-terminal authority protects nothing.
func TestServeWithoutProtectedTerminalsProtectsNothing(t *testing.T) {
	src := newPerIDSource(sshID)
	mgr := NewManager(src, nil, testLogger(), WithHeartbeat(0))
	defer mgr.Close()
	conn := serveTestConn(t, mgr)
	conn.in <- clientMsg{Ch: chTerminal, ID: sshID, Type: msgOpen}
	recv(t, conn, chTerminal, msgOpened, time.Second)
}

// The guard is asked again at every probe and attach: a terminal that became
// protected after a tokenless open is reported gone and never attached again.
func TestGuardedSourceRefusesTerminalProtectedAfterOpen(t *testing.T) {
	src := newPerIDSource(plainID)
	mgr, set := newProtectedTestManager(src)
	defer mgr.Close()
	guard := guardedSource{Source: src, mgr: mgr}
	ctx := context.Background()

	if alive, err := guard.IsAlive(ctx, ports.RuntimeHandle{ID: plainID}); !alive || err != nil {
		t.Fatalf("IsAlive before protection = %v, %v; want true", alive, err)
	}
	set.protect(plainID)
	if alive, err := guard.IsAlive(ctx, ports.RuntimeHandle{ID: plainID}); alive || err != nil {
		t.Fatalf("IsAlive after protection = %v, %v; want false, nil", alive, err)
	}
	if stream, err := guard.Attach(ctx, ports.RuntimeHandle{ID: plainID}, 24, 80); stream != nil || err == nil {
		t.Fatalf("Attach after protection = %v, %v; want a refusal", stream, err)
	}
	if _, attaches := src.calls(plainID); attaches != 0 {
		t.Fatalf("runtime attaches = %d, want 0", attaches)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
