package shellterm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

const (
	testSSHToken       = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testSSHKnownHosts  = `G:\AppData\Coding Tools\integrations\ssh-known-hosts`
	testSSHProgram     = `C:\Windows\System32\OpenSSH\ssh.exe`
	testSSHKeyFile     = `C:\keys\db_ed25519`
	testSSHFirstHandle = "shellterm-ssh-0000000000000001"
)

// The two command lines the contract fixes (argv after the program).
const (
	sshSample1JSON = `["-F","none","-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile=\"G:/AppData/Coding Tools/integrations/ssh-known-hosts\"","-o","GlobalKnownHostsFile=none","-o","UpdateHostKeys=no","-o","VerifyHostKeyDNS=no","-o","ForwardAgent=no","-o","ForwardX11=no","-o","ForwardX11Trusted=no","-o","ClearAllForwardings=yes","-o","Tunnel=no","-o","PermitLocalCommand=no","-o","ProxyCommand=none","-o","ControlMaster=no","-o","ControlPath=none","-o","Compression=no","-o","IdentityAgent=none","-o","AddKeysToAgent=no","-o","EscapeChar=none","-o","EnableEscapeCommandline=no","-o","ConnectTimeout=15","-o","ConnectionAttempts=1","-o","ServerAliveInterval=30","-o","ServerAliveCountMax=3","-o","BatchMode=no","-t","-p","22","-l","deploy","--","build.example.com"]`
	sshSample2JSON = `["-F","none","-o","StrictHostKeyChecking=yes","-o","UserKnownHostsFile=\"G:/AppData/Coding Tools/integrations/ssh-known-hosts\"","-o","GlobalKnownHostsFile=none","-o","CheckHostIP=no","-o","UpdateHostKeys=no","-o","VerifyHostKeyDNS=no","-o","ForwardAgent=no","-o","ForwardX11=no","-o","ForwardX11Trusted=no","-o","ClearAllForwardings=yes","-o","Tunnel=no","-o","PermitLocalCommand=no","-o","ProxyCommand=none","-o","ControlMaster=no","-o","ControlPath=none","-o","Compression=no","-o","IdentityAgent=none","-o","AddKeysToAgent=no","-o","EscapeChar=none","-o","ConnectTimeout=15","-o","ConnectionAttempts=1","-o","ServerAliveInterval=30","-o","ServerAliveCountMax=3","-o","BatchMode=no","-o","IdentitiesOnly=yes","-i","C:\\keys\\db_ed25519","-t","-p","2222","-l","admin","--","2001:db8::7"]`
)

func sampleArgv(t *testing.T, raw string) []string {
	t.Helper()
	var argv []string
	if err := json.Unmarshal([]byte(raw), &argv); err != nil {
		t.Fatalf("sample argv: %v", err)
	}
	return argv
}

type fakeFileInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeFiles answers the SSH rules' file checks: a path is a regular file, a
// directory, or not there.
type fakeFiles struct {
	mu      sync.Mutex
	entries map[string]fs.FileMode
}

func (f *fakeFiles) stat(path string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mode, ok := f.entries[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return fakeFileInfo{name: filepath.Base(path), mode: mode}, nil
}

func (f *fakeFiles) set(path string, mode fs.FileMode) {
	f.mu.Lock()
	f.entries[path] = mode
	f.mu.Unlock()
}

func (f *fakeFiles) remove(path string) {
	f.mu.Lock()
	delete(f.entries, path)
	f.mu.Unlock()
}

type sshTestRig struct {
	svc   *Service
	rt    *fakeShellRuntime
	store *fakeShellTerminalStore
	files *fakeFiles
	// windowsDir answers the kernel's Windows folder; environ is the daemon's
	// environment as ssh's is built from it.
	windowsDir string
	environ    []string
	logs       *bytes.Buffer
}

func newSSHTestRig(t *testing.T) *sshTestRig {
	t.Helper()
	rig := &sshTestRig{
		rt:    newFakeShellRuntime(),
		store: &fakeShellTerminalStore{},
		files: &fakeFiles{entries: map[string]fs.FileMode{
			testSSHKnownHosts: 0,
			testSSHProgram:    0,
			testSSHKeyFile:    0,
		}},
		windowsDir: `C:\Windows`,
		environ:    []string{`SystemRoot=C:\Windows`, `USERPROFILE=C:\Users\person`},
		logs:       &bytes.Buffer{},
	}
	rig.svc = newTestService(rig.rt, rig.store, &fakeProjectRootLocator{})
	WithSSHTerminal(SSHTerminalSettings{KnownHostsFile: testSSHKnownHosts, Token: testSSHToken})(rig.svc)
	rig.svc.log = slog.New(slog.NewTextHandler(rig.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rig.svc.dataDir = t.TempDir()
	// The route runs only on Windows; the rig is Windows whatever runs it.
	rig.svc.goos = "windows"
	rig.svc.windowsDirectory = func() (string, error) {
		if rig.windowsDir == "" {
			return "", errors.New("no Windows folder")
		}
		return rig.windowsDir, nil
	}
	rig.svc.environ = func() []string { return rig.environ }
	rig.svc.statFile = rig.files.stat
	var n int
	rig.svc.newSSHHandleID = func() (string, error) {
		n++
		return fmt.Sprintf("%s%016x", sshHandlePrefix, n), nil
	}
	return rig
}

func sshAccess() context.Context { return WithSSHTerminalAccess(context.Background()) }

func intRef(v int) *int { return &v }

func clientVersion(major, minor int) *SSHClientVersionInput {
	return &SSHClientVersionInput{Major: intRef(major), Minor: intRef(minor)}
}

func sshSample1Input() OpenSSHTerminalInput {
	return OpenSSHTerminalInput{Host: "build.example.com", Port: intRef(22), User: "deploy", Title: "Build server", Client: clientVersion(9, 5)}
}

func sshSample2Input() OpenSSHTerminalInput {
	return OpenSSHTerminalInput{Host: "2001:db8::7", Port: intRef(2222), User: "admin", IdentityFile: testSSHKeyFile, Title: "Database", Client: clientVersion(8, 1)}
}

func apiErrorOf(t *testing.T, err error) *apierr.Error {
	t.Helper()
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want an *apierr.Error", err)
	}
	return apiErr
}

func assertSSHRefusal(t *testing.T, err error, kind apierr.Kind, code, message string) {
	t.Helper()
	apiErr := apiErrorOf(t, err)
	if apiErr.Kind != kind || apiErr.Code != code {
		t.Fatalf("error = kind %v code %q (%q), want kind %v code %q", apiErr.Kind, apiErr.Code, apiErr.Message, kind, code)
	}
	if message != "" && apiErr.Message != message {
		t.Fatalf("message = %q, want %q", apiErr.Message, message)
	}
}

func TestSSHTerminalArgvMatchesContractSamples(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target sshTerminalTarget
		want   string
	}{
		{"sample 1", sshTerminalTarget{host: "build.example.com", port: 22, user: "deploy", client: &sshClientVersion{9, 5}}, sshSample1JSON},
		{"sample 2", sshTerminalTarget{host: "2001:db8::7", port: 2222, user: "admin", identityFile: testSSHKeyFile, client: &sshClientVersion{8, 1}}, sshSample2JSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sshTerminalArgv(tc.target, testSSHKnownHosts, true)
			if !ok {
				t.Fatal("sshTerminalArgv refused the pinned file path")
			}
			if want := sampleArgv(t, tc.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("argv =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// The full open path: the program, the exact argv, the environment, the
// private working folder and a transient record.
func TestOpenSSHTerminalStartsContractCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input OpenSSHTerminalInput
		argv  string
		title string
	}{
		{"sample 1", sshSample1Input(), sshSample1JSON, "Build server"},
		{"sample 2", sshSample2Input(), sshSample2JSON, "Database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSSHTestRig(t)
			terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), tc.input)
			if err != nil {
				t.Fatalf("OpenSSHTerminal: %v", err)
			}
			if len(rig.rt.created) != 1 {
				t.Fatalf("runtime creates = %d, want 1", len(rig.rt.created))
			}
			cfg := rig.rt.created[0]
			if want := append([]string{testSSHProgram}, sampleArgv(t, tc.argv)...); !reflect.DeepEqual(cfg.Argv, want) {
				t.Fatalf("argv =\n%q\nwant\n%q", cfg.Argv, want)
			}
			want := map[string]string{"SystemRoot": `C:\Windows`, "USERPROFILE": `C:\Users\person`, "SSH_ASKPASS_REQUIRE": "never"}
			if !reflect.DeepEqual(cfg.Env, want) || !cfg.EnvExact {
				t.Fatalf("env = %#v (exact %v), want exactly %#v", cfg.Env, cfg.EnvExact, want)
			}
			wantDir := filepath.Join(rig.svc.dataDir, sshWorkingDirectoryName)
			if cfg.WorkspacePath != wantDir {
				t.Fatalf("working dir = %q, want the shared SSH folder %q", cfg.WorkspacePath, wantDir)
			}
			if string(cfg.SessionID) != testSSHFirstHandle || terminal.HandleID != testSSHFirstHandle || terminal.Title != tc.title {
				t.Fatalf("terminal = %+v (runtime id %q)", terminal, cfg.SessionID)
			}
			if len(rig.store.records) != 1 || !rig.store.records[0].Transient || rig.store.records[0].ProjectID != "" || rig.store.records[0].SessionID != "" {
				t.Fatalf("records = %+v, want one transient record with no project or session", rig.store.records)
			}
			if !rig.svc.IsProtectedTerminal(terminal.HandleID) {
				t.Fatal("the opened SSH terminal is not protected")
			}
		})
	}
}

// The client version only adds CheckHostIP=no (before 8.5) and
// EnableEscapeCommandline=no (9.2 and later); an absent version is current.
func TestSSHTerminalArgvFollowsClientVersion(t *testing.T) {
	for _, tc := range []struct {
		client         *sshClientVersion
		checkHostIP    bool
		escapeDisabled bool
	}{
		{&sshClientVersion{7, 9}, true, false},
		{&sshClientVersion{8, 4}, true, false},
		{&sshClientVersion{8, 5}, false, false},
		{&sshClientVersion{9, 1}, false, false},
		{&sshClientVersion{9, 2}, false, true},
		{&sshClientVersion{9, 5}, false, true},
		{&sshClientVersion{10, 0}, false, true},
		{nil, false, true},
	} {
		argv, ok := sshTerminalArgv(sshTerminalTarget{host: "build.example.com", port: 22, user: "deploy", client: tc.client}, testSSHKnownHosts, true)
		if !ok {
			t.Fatal("argv refused")
		}
		joined := strings.Join(argv, "\x00")
		if got := strings.Contains(joined, "CheckHostIP=no"); got != tc.checkHostIP {
			t.Errorf("client %v: CheckHostIP=no present = %v, want %v", tc.client, got, tc.checkHostIP)
		}
		if got := strings.Contains(joined, "EnableEscapeCommandline=no"); got != tc.escapeDisabled {
			t.Errorf("client %v: EnableEscapeCommandline=no present = %v, want %v", tc.client, got, tc.escapeDisabled)
		}
		if argv[len(argv)-1] != "build.example.com" || argv[len(argv)-2] != "--" {
			t.Errorf("client %v: argv does not end with -- host: %q", tc.client, argv)
		}
	}
}

func TestOpenSSHTerminalValidation(t *testing.T) {
	type mutation func(*OpenSSHTerminalInput)
	host := func(v string) mutation { return func(in *OpenSSHTerminalInput) { in.Host = v } }
	port := func(v *int) mutation { return func(in *OpenSSHTerminalInput) { in.Port = v } }
	user := func(v string) mutation { return func(in *OpenSSHTerminalInput) { in.User = v } }
	key := func(v string) mutation { return func(in *OpenSSHTerminalInput) { in.IdentityFile = v } }
	title := func(v string) mutation { return func(in *OpenSSHTerminalInput) { in.Title = v } }
	client := func(v *SSHClientVersionInput) mutation { return func(in *OpenSSHTerminalInput) { in.Client = v } }
	const (
		hostIP      = "Host: this looks like an IP address but is not a valid one. Use four numbers from 0 to 255, like 192.0.2.10."
		hostIPv6    = "Host: this is not a valid IPv6 address. Use hex digits and colons only, like 2001:db8::1."
		hostStored  = "Host: use the stored lower-case host name (an IPv6 address in its short form)."
		hostEmbeds  = "Host: this IPv6 address has an IPv4 address inside it, which ssh writes another way. Enter the server's IPv4 address, or another IPv6 address of it."
		hostSpaces  = "Host: a host name cannot contain spaces, tabs or line breaks. Remove them and try again."
		hostEnds    = "Host: a host name starts and ends with a letter or a digit."
		portDigits  = "Port: use digits only, from 1 to 65535. Leave it empty for 22."
		portRange   = "Port: use a number from 1 to 65535. Leave it empty for 22."
		userSpaces  = "User: a user name cannot contain spaces, tabs or line breaks."
		userPattern = "User: use letters, digits, dots, underscores and hyphens only, starting with a letter, a digit or an underscore."
		keyDrive    = `Key file: enter the full path, starting with a drive letter (for example C:\Users\you\.ssh\id_ed25519).`
		keyPub      = "Key file: choose the private key, not the .pub file next to it."
		keyExpand   = "Key file: the path cannot contain % or ${ (ssh would replace them). Move or rename the file."
		keyMissing  = "Key file: no file was found at that path, so the terminal was not opened. Put the key file back, or remove this host and add it again."
		titleEmpty  = "Title: enter a name for this terminal."
		titleHidden = "Title: a title cannot contain line breaks or invisible characters."
		clientBad   = "Client: send the OpenSSH version as two whole numbers from 0 to 999 (major and minor)."
	)
	for _, tc := range []struct {
		name   string
		change mutation
		want   string
	}{
		{"host empty", host(""), "Host: enter a host name or an IP address."},
		{"host too long", host(strings.Repeat("a", 254)), "Host: a host name can be at most 253 characters."},
		{"host space", host("build example.com"), hostSpaces},
		{"host tab", host("build\texample.com"), hostSpaces},
		{"host no-break space", host("build\u00a0example.com"), hostSpaces},
		{"host line break", host("build\nexample.com"), hostSpaces},
		{"host non-ASCII", host("bücher.example"), "Host: use plain ASCII letters and digits only. For an international name, enter its xn-- form."},
		{"host ssh URL", host("ssh://build.example.com"), "Host: enter the host name only, not an ssh:// address. The user and the port have their own fields."},
		{"host with user", host("deploy@build.example.com"), "Host: enter the host name only. The user has its own field."},
		{"host leading hyphen", host("-oProxyCommand=calc"), "Host: a host name cannot start with a hyphen."},
		{"host brackets", host("[2001:db8::7]"), "Host: enter an IPv6 address without square brackets. The port has its own field."},
		{"host zone", host("fe80::1%eth0"), "Host: an IPv6 address with a zone (the part after %) is not supported."},
		{"host underscore", host("build_server"), `Host: "_" is not allowed in a host name. Use letters, digits, dots and hyphens.`},
		{"host double quote", host(`build"x`), "Host: A double quote is not allowed in a host name. Use letters, digits, dots and hyphens."},
		{"host single quote", host("build'x"), "Host: A single quote is not allowed in a host name. Use letters, digits, dots and hyphens."},
		{"host backtick", host("build`x"), "Host: A backtick is not allowed in a host name. Use letters, digits, dots and hyphens."},
		{"host semicolon", host("build;calc"), `Host: ";" is not allowed in a host name. Use letters, digits, dots and hyphens.`},
		{"host with port", host("build.example.com:22"), "Host: enter the host name only. The port has its own field."},
		{"host bad IPv6", host("2001:db8::g"), hostIPv6},
		{"host nine IPv6 groups", host("1:2:3:4:5:6:7:8:9"), hostIPv6},
		{"host two ellipses", host("1::2::3"), hostIPv6},
		{"host mapped IPv4", host("::ffff:c000:201"), hostEmbeds},
		{"host compatible IPv4", host("::1:0"), hostEmbeds},
		{"host ISATAP", host("fe80::200:5efe:c000:201"), hostEmbeds},
		{"host IPv6 long form", host("2001:0db8:0000:0000:0000:0000:0000:0007"), hostStored},
		{"host IPv6 upper case", host("2001:DB8::7"), hostStored},
		{"host upper case name", host("Build.Example.com"), hostStored},
		{"host short IPv4", host("127.1"), hostIP},
		{"host hex IPv4", host("0x7f.1"), hostIP},
		{"host decimal IPv4", host("2130706433"), hostIP},
		{"host hex last label", host("a.0x7f"), hostIP},
		{"host IPv4 out of range", host("256.1.1.1"), hostIP},
		{"host IPv4 leading zero", host("192.0.2.010"), hostIP},
		{"host leading dot", host(".example.com"), hostEnds},
		{"host trailing dot", host("example.com."), hostEnds},
		{"host trailing hyphen", host("example-"), hostEnds},
		{"host two dots", host("a..b"), "Host: a host name cannot have two dots in a row."},
		{"host long label", host(strings.Repeat("a", 64) + ".example"), "Host: each part of a host name (between dots) can be at most 63 characters."},
		{"port zero", port(intRef(0)), portRange},
		{"port above range", port(intRef(65536)), portRange},
		{"port five nines", port(intRef(99999)), portRange},
		{"port six digits", port(intRef(100000)), portDigits},
		{"port negative", port(intRef(-1)), portDigits},
		{"user empty", user(""), "User: enter the user name you sign in with on the server."},
		{"user too long", user(strings.Repeat("u", 33)), "User: a user name can be at most 32 characters."},
		{"user space", user("de ploy"), userSpaces},
		{"user line separator", user("de\u2028ploy"), userSpaces},
		{"user control", user("de\x00ploy"), userSpaces},
		{"user at", user("deploy@build"), "User: enter the user name only, without @ and the host. The host has its own field."},
		{"user domain", user(`CORP\deploy`), `User: Windows domain logins (DOMAIN\name) are not supported. Enter the user name only.`},
		{"user leading hyphen", user("-oProxyCommand=calc"), "User: a user name cannot start with a hyphen."},
		{"user leading dot", user(".deploy"), userPattern},
		{"user semicolon", user("de;ploy"), userPattern},
		{"user non-ASCII", user("dé"), userPattern},
		{"key relative", key(`keys\id_ed25519`), keyDrive},
		{"key UNC", key(`\\server\share\id_ed25519`), keyDrive},
		{"key pub", key(`C:\keys\id_ed25519.pub`), keyPub},
		{"key pub with trailing dot and space", key(`C:\keys\id_ed25519.PUB. `), keyPub},
		{"key pub after normalising", key(`C:\keys\id_ed25519.pub\.`), keyPub},
		{"key double quote", key(`C:\keys\"id"`), "Key file: the path cannot contain double quotes."},
		{"key percent", key(`C:\keys\id%h`), keyExpand},
		{"key expansion", key(`C:\keys\${HOME}`), keyExpand},
		{"key control", key("C:\\keys\\id\nx"), "Key file: the path cannot contain line breaks or control characters."},
		{"key too long", key(`C:\` + strings.Repeat("k", 1022)), "Key file: the path is too long."},
		{"key missing", key(`C:\keys\missing`), keyMissing},
		{"key is a folder", key(`C:\keys`), keyMissing},
		{"title empty", title(""), titleEmpty},
		{"title blank", title(" \t\u3000\ufeff"), titleEmpty},
		{"title too long", title(strings.Repeat("t", 81)), "Title: use at most 80 characters."},
		{"title line break", title("Build\nserver"), titleHidden},
		{"title zero-width space", title("Build\u200bserver"), titleHidden},
		{"title line separator inside", title("Build\u2028server"), titleHidden},
		{"title unassigned code point", title("Build\u0378"), titleHidden},
		{"client without major", client(&SSHClientVersionInput{Minor: intRef(5)}), clientBad},
		{"client without minor", client(&SSHClientVersionInput{Major: intRef(9)}), clientBad},
		{"client negative", client(clientVersion(-1, 0)), clientBad},
		{"client too large", client(clientVersion(1000, 0)), clientBad},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSSHTestRig(t)
			rig.files.set(`C:\keys`, fs.ModeDir)
			in := sshSample1Input()
			tc.change(&in)
			_, err := rig.svc.OpenSSHTerminal(sshAccess(), in)
			assertSSHRefusal(t, err, apierr.KindInvalid, SSHTerminalInvalidCode, tc.want)
			if len(rig.rt.created) != 0 || len(rig.store.records) != 0 {
				t.Fatal("a refused request started a runtime or wrote a record")
			}
		})
	}
}

func TestOpenSSHTerminalAcceptsAppForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input func(*OpenSSHTerminalInput)
		check func(t *testing.T, argv []string, title string)
	}{
		{"IPv4", func(in *OpenSSHTerminalInput) { in.Host = "192.0.2.10" }, nil},
		{"IPv6 loopback", func(in *OpenSSHTerminalInput) { in.Host = "::1" }, nil},
		{"punycode", func(in *OpenSSHTerminalInput) { in.Host = "xn--bcher-kva.example" }, nil},
		{"port omitted", func(in *OpenSSHTerminalInput) { in.Port = nil }, func(t *testing.T, argv []string, _ string) {
			if !strings.Contains(strings.Join(argv, " "), " -p 22 ") {
				t.Fatalf("argv = %q, want port 22", argv)
			}
		}},
		{"port bounds", func(in *OpenSSHTerminalInput) { in.Port = intRef(65535) }, nil},
		{"user forms", func(in *OpenSSHTerminalInput) { in.User = "_svc.deploy-1" }, nil},
		{"key with forward slashes", func(in *OpenSSHTerminalInput) { in.IdentityFile = "C:/keys/db_ed25519" }, func(t *testing.T, argv []string, _ string) {
			if !strings.Contains(strings.Join(argv, "\x00"), "\x00-i\x00"+testSSHKeyFile+"\x00") {
				t.Fatalf("argv = %q, want the normalised key path", argv)
			}
		}},
		{"key with dot segments", func(in *OpenSSHTerminalInput) { in.IdentityFile = `C:\keys\..\keys\.\db_ed25519` }, func(t *testing.T, argv []string, _ string) {
			if !strings.Contains(strings.Join(argv, "\x00"), "\x00-i\x00"+testSSHKeyFile+"\x00") {
				t.Fatalf("argv = %q, want the normalised key path", argv)
			}
		}},
		{"title trimmed", func(in *OpenSSHTerminalInput) { in.Title = "  Build server\u2028 " }, func(t *testing.T, _ []string, title string) {
			if title != "Build server" {
				t.Fatalf("title = %q, want it trimmed as the app trims a label", title)
			}
		}},
		{"title of 80 characters beyond the BMP", func(in *OpenSSHTerminalInput) { in.Title = strings.Repeat("\U0001F5A5", 80) }, nil},
		{"client absent", func(in *OpenSSHTerminalInput) { in.Client = nil }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSSHTestRig(t)
			in := sshSample1Input()
			tc.input(&in)
			terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), in)
			if err != nil {
				t.Fatalf("OpenSSHTerminal: %v", err)
			}
			if tc.check != nil {
				tc.check(t, rig.rt.created[0].Argv, terminal.Title)
			}
		})
	}
}

func TestOpenSSHTerminalRefusalsInOrder(t *testing.T) {
	bad := sshSample1Input()
	bad.Host = "-oProxyCommand=calc"
	lanWithAccess := requestscope.WithLAN(sshAccess())

	t.Run("LAN caller first, even with the route off", func(t *testing.T) {
		rig := newSSHTestRig(t)
		WithSSHTerminal(SSHTerminalSettings{})(rig.svc)
		_, err := rig.svc.OpenSSHTerminal(lanWithAccess, bad)
		assertSSHRefusal(t, err, apierr.KindForbidden, SSHTerminalLocalOnlyCode, "")
	})
	t.Run("route off before the capability", func(t *testing.T) {
		for _, settings := range []SSHTerminalSettings{
			{},
			{KnownHostsFile: testSSHKnownHosts},
			{Token: testSSHToken},
			{KnownHostsFile: testSSHKnownHosts, Token: testSSHToken[:31]},
		} {
			rig := newSSHTestRig(t)
			WithSSHTerminal(settings)(rig.svc)
			_, err := rig.svc.OpenSSHTerminal(context.Background(), bad)
			assertSSHRefusal(t, err, apierr.KindNotFound, SSHTerminalDisabledCode, "")
			if rig.svc.SSHTerminalEnabled() || rig.svc.SSHTerminalTokenMatches(testSSHToken) {
				t.Fatal("route reported on")
			}
		}
	})
	t.Run("capability before the body", func(t *testing.T) {
		rig := newSSHTestRig(t)
		_, err := rig.svc.OpenSSHTerminal(context.Background(), bad)
		assertSSHRefusal(t, err, apierr.KindForbidden, SSHTerminalForbiddenCode, "This request is not allowed.")
	})
	t.Run("body before the pinned file", func(t *testing.T) {
		rig := newSSHTestRig(t)
		rig.files.remove(testSSHKnownHosts)
		_, err := rig.svc.OpenSSHTerminal(sshAccess(), bad)
		assertSSHRefusal(t, err, apierr.KindInvalid, SSHTerminalInvalidCode, "Host: a host name cannot start with a hyphen.")
	})
	t.Run("pinned file before the program", func(t *testing.T) {
		for _, setUp := range []func(*sshTestRig){
			func(r *sshTestRig) { r.files.remove(testSSHKnownHosts) },
			func(r *sshTestRig) { r.files.set(testSSHKnownHosts, fs.ModeDir) },
			func(r *sshTestRig) {
				WithSSHTerminal(SSHTerminalSettings{KnownHostsFile: `G:\App"Data\ssh-known-hosts`, Token: testSSHToken})(r.svc)
				r.files.set(`G:\App"Data\ssh-known-hosts`, 0)
			},
			func(r *sshTestRig) {
				WithSSHTerminal(SSHTerminalSettings{KnownHostsFile: `G:\${HOME}\ssh-known-hosts`, Token: testSSHToken})(r.svc)
				r.files.set(`G:\${HOME}\ssh-known-hosts`, 0)
			},
		} {
			rig := newSSHTestRig(t)
			setUp(rig)
			rig.files.remove(testSSHProgram)
			_, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
			assertSSHRefusal(t, err, apierr.KindConflict, SSHTerminalNoPinnedKeysCode, "")
		}
	})
	t.Run("ssh program missing", func(t *testing.T) {
		for _, setUp := range []func(*sshTestRig){
			func(r *sshTestRig) { r.files.remove(testSSHProgram) },
			func(r *sshTestRig) { r.files.set(testSSHProgram, fs.ModeDir) },
		} {
			rig := newSSHTestRig(t)
			setUp(rig)
			_, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
			assertSSHRefusal(t, err, apierr.KindConflict, SSHTerminalNoClientCode, "OpenSSH was not found on this computer, so the terminal was not opened.")
			if len(rig.rt.created) != 0 {
				t.Fatal("a refused request started a runtime")
			}
		}
	})
}

func TestSSHTerminalTokenMatching(t *testing.T) {
	rig := newSSHTestRig(t)
	for _, tc := range []struct {
		presented string
		want      bool
	}{
		{testSSHToken, true},
		{"", false},
		{testSSHToken[:63], false},
		{testSSHToken + "0", false},
		{strings.ToUpper(testSSHToken), false},
		{" " + testSSHToken, false},
	} {
		if got := rig.svc.SSHTerminalTokenMatches(tc.presented); got != tc.want {
			t.Errorf("SSHTerminalTokenMatches(len %d) = %v, want %v", len(tc.presented), got, tc.want)
		}
	}
	if HasSSHTerminalAccess(context.Background()) {
		t.Fatal("a plain context has SSH terminal access")
	}
	if HasSSHTerminalAccess(requestscope.WithLAN(sshAccess())) {
		t.Fatal("a LAN context has SSH terminal access")
	}
	if !HasSSHTerminalAccess(sshAccess()) {
		t.Fatal("a marked loopback context has no SSH terminal access")
	}
}

// ssh is started from the Windows folder the kernel reports; a folder that is
// not a plain drive path, or that could not be read, means no client.
func TestSSHProgramPathUsesTheKernelWindowsFolder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dir     string
		err     error
		want    string
		ok      bool
		windows bool
	}{
		{"kernel folder", `C:\Windows`, nil, testSSHProgram, true, true},
		{"upper case", `C:\WINDOWS`, nil, `C:\WINDOWS\System32\OpenSSH\ssh.exe`, true, true},
		{"trailing separator", `D:\Win\`, nil, `D:\Win\System32\OpenSSH\ssh.exe`, true, true},
		{"with a space", `D:\Win dows`, nil, `D:\Win dows\System32\OpenSSH\ssh.exe`, true, true},
		{"unreadable", "", errors.New("no folder"), "", false, true},
		{"empty", "", nil, "", false, true},
		{"relative", `Windows`, nil, "", false, true},
		{"UNC", `\\server\share\Windows`, nil, "", false, true},
		{"quote", `C:\Win"dows`, nil, "", false, true},
		{"control character", "C:\\Win\ndows", nil, "", false, true},
		{"not Windows", `C:\Windows`, nil, "/usr/bin/ssh", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sshProgramPath(tc.windows, func() (string, error) { return tc.dir, tc.err })
			if got != tc.want || ok != tc.ok {
				t.Fatalf("sshProgramPath = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// SystemRoot and windir in the environment do not choose the program.
func TestOpenSSHTerminalIgnoresSystemRootForTheProgram(t *testing.T) {
	rig := newSSHTestRig(t)
	rig.environ = append(rig.environ, `SystemRoot=D:\Planted`, `SYSTEMROOT=D:\Planted`, `windir=D:\Planted`)
	rig.files.set(`D:\Planted\System32\OpenSSH\ssh.exe`, 0)
	rig.windowsDir = `E:\Win`
	rig.files.remove(testSSHProgram)
	rig.files.set(`E:\Win\System32\OpenSSH\ssh.exe`, 0)
	if _, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input()); err != nil {
		t.Fatalf("OpenSSHTerminal: %v", err)
	}
	if got := rig.rt.created[0].Argv[0]; got != `E:\Win\System32\OpenSSH\ssh.exe` {
		t.Fatalf("program = %q, want the kernel folder's ssh", got)
	}
}

func TestOpenSSHTerminalWithoutAWindowsFolderHasNoClient(t *testing.T) {
	rig := newSSHTestRig(t)
	rig.windowsDir = ""
	_, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
	assertSSHRefusal(t, err, apierr.KindConflict, SSHTerminalNoClientCode, "")
}
func TestSSHOptionPathFollowsAppRules(t *testing.T) {
	for _, tc := range []struct {
		file    string
		windows bool
		want    string
		ok      bool
	}{
		{testSSHKnownHosts, true, `"G:/AppData/Coding Tools/integrations/ssh-known-hosts"`, true},
		{`C:\data\100%\known`, true, `"C:/data/100%%/known"`, true},
		{`\\server\share\known`, true, `"//server/share/known"`, true},
		{`relative\known`, true, "", false},
		{`C:\data\"known"`, true, "", false},
		{"C:\\data\\kno\twn", true, "", false},
		{`C:\data\${HOME}`, true, "", false},
		{"/home/u/known_hosts", false, `"/home/u/known_hosts"`, true},
		{`/home/u\known`, false, "", false},
		{"home/u/known", false, "", false},
	} {
		got, ok := sshOptionPath(tc.file, tc.windows)
		if ok != tc.ok || got != tc.want {
			t.Errorf("sshOptionPath(%q, windows=%v) = %q, %v; want %q, %v", tc.file, tc.windows, got, ok, tc.want, tc.ok)
		}
	}
}

func TestValidateSSHIdentityFileOnPosix(t *testing.T) {
	for _, tc := range []struct {
		input, want, problem string
	}{
		{"/home/u/.ssh/id_ed25519", "/home/u/.ssh/id_ed25519", ""},
		{"/home/u//.ssh/./id_ed25519", "/home/u/.ssh/id_ed25519", ""},
		{"/home/u/.ssh/../.ssh/id_ed25519", "/home/u/.ssh/id_ed25519", ""},
		{"home/u/id", "", "Key file: enter the full path, starting with /."},
		{"/home/u/id.pub", "", "Key file: choose the private key, not the .pub file next to it."},
		{"/home/u/id.pub/..", "/home/u", ""},
		{"/home/u/x/../id.pub", "", "Key file: choose the private key, not the .pub file next to it."},
		{"C:\\keys\\id", "", "Key file: enter the full path, starting with /."},
	} {
		got, problem := validateSSHIdentityFile(tc.input, false)
		if got != tc.want || problem != tc.problem {
			t.Errorf("validateSSHIdentityFile(%q) = %q, %q; want %q, %q", tc.input, got, problem, tc.want, tc.problem)
		}
	}
}

// Only the minted shape is protected by its id, so a project whose session
// prefix is "shellterm-ssh" keeps its own terminals.
func TestSSHHandleProtection(t *testing.T) {
	rig := newSSHTestRig(t)
	for id, want := range map[string]bool{
		"shellterm-ssh-0123456789abcdef":             true,
		"ptyhost-v1:shellterm-ssh-0123456789abcdef":  true,
		"SHELLTERM-SSH-0123456789ABCDEF":             true,
		" shellterm-ssh-0123456789abcdef\n":          true,
		"ptyhost-v1: shellterm-ssh-0123456789abcdef": true,
		"shellterm-ssh-0123abcd":                     false,
		"shellterm-ssh-0123456789abcdef0":            false,
		"shellterm-ssh-0123456789abcdeg":             false,
		"shellterm-ssh-1":                            false,
		"shellterm-ssh-agent-7":                      false,
		"shellterm-ssh":                              false,
		"shellterm-0123456789abcdef":                 false,
		"shellterm-sshx":                             false,
		"ptyhost-v1:shellterm-0123abcd":              false,
		"":                                           false,
	} {
		if got := rig.svc.IsProtectedTerminal(id); got != want {
			t.Errorf("IsProtectedTerminal(%q) = %v, want %v", id, got, want)
		}
	}
}

// A terminal named with the SSH prefix but not the minted shape (an agent
// session of a project prefixed "shellterm-ssh") is listed, renamed and closed
// as before, without the capability.
func TestPrefixedNonSSHTerminalIsNotLockedOut(t *testing.T) {
	rig := newSSHTestRig(t)
	const agentHandle = "shellterm-ssh-7"
	rig.store.records = append(rig.store.records, ShellTerminalRecord{HandleID: agentHandle, Title: "Terminal 1", AppRunID: testAppRunID})
	rig.rt.aliveByHandle[agentHandle] = true
	listed, err := rig.svc.ListShellTerminalsForCurrentAppRun(context.Background())
	if err != nil || len(listed) != 1 || listed[0].HandleID != agentHandle {
		t.Fatalf("list = %+v, %v; want the prefixed terminal", listed, err)
	}
	if _, err := rig.svc.RenameShellTerminal(context.Background(), agentHandle, "Renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := rig.svc.CloseShellTerminal(context.Background(), agentHandle); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A runtime that answers with an id of its own: that id is protected the
// moment the runtime exists, before the record that lists it is written.
func TestOpenSSHTerminalProtectsTheRuntimesOwnHandleBeforeItIsListed(t *testing.T) {
	rig := newSSHTestRig(t)
	rig.rt.handlePrefix = "x-"
	var protectedBeforeInsert bool
	rig.store.insertHook = func(rec ShellTerminalRecord) { protectedBeforeInsert = rig.svc.IsProtectedTerminal(rec.HandleID) }
	terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
	if err != nil {
		t.Fatalf("OpenSSHTerminal: %v", err)
	}
	if terminal.HandleID != "x-"+testSSHFirstHandle || isSSHHandleID(terminal.HandleID) {
		t.Fatalf("handle = %q, want the runtime's own id", terminal.HandleID)
	}
	if !protectedBeforeInsert {
		t.Fatal("the runtime's handle was not protected before its record was written")
	}
	listed, err := rig.svc.ListShellTerminalsForCurrentAppRun(context.Background())
	if err != nil || len(listed) != 0 {
		t.Fatalf("list = %+v, %v; want the SSH terminal hidden", listed, err)
	}
	// Closed with the capability: it leaves the set.
	if err := rig.svc.CloseShellTerminal(sshAccess(), terminal.HandleID); err != nil {
		t.Fatalf("CloseShellTerminal: %v", err)
	}
	if rig.svc.IsProtectedTerminal(terminal.HandleID) {
		t.Fatal("a closed SSH terminal stayed in the set")
	}
}

func TestListShellTerminalsHidesSSHTerminals(t *testing.T) {
	rig := newSSHTestRig(t)
	if _, err := rig.svc.OpenCommandTerminal(context.Background(), OpenCommandTerminalInput{Argv: []string{"pi"}, Title: "Log in to Pi"}); err != nil {
		t.Fatalf("OpenCommandTerminal: %v", err)
	}
	if _, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input()); err != nil {
		t.Fatalf("OpenSSHTerminal: %v", err)
	}
	// A record left by an earlier daemon of this app run: the set is empty, the
	// id still marks it.
	rig.store.records = append(rig.store.records, ShellTerminalRecord{HandleID: "shellterm-ssh-00000000000000ff", Title: "Earlier", AppRunID: testAppRunID, Transient: true})
	rig.rt.aliveByHandle["shellterm-ssh-00000000000000ff"] = true
	rig.rt.childExited = true // an exited child is pruned for a listed terminal, never for an SSH one

	listed, err := rig.svc.ListShellTerminalsForCurrentAppRun(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, terminal := range listed {
		if rig.svc.IsProtectedTerminal(terminal.HandleID) {
			t.Fatalf("list shows SSH terminal %q", terminal.HandleID)
		}
	}
	for _, destroyed := range rig.rt.destroyed {
		if isSSHHandleID(destroyed) {
			t.Fatalf("list pruned SSH terminal %q", destroyed)
		}
	}
	sshRecords := 0
	for _, rec := range rig.store.records {
		if isSSHHandleID(rec.HandleID) {
			sshRecords++
		}
	}
	if sshRecords != 2 {
		t.Fatalf("SSH records after list = %d, want both kept", sshRecords)
	}
}

func TestNextShellTerminalTitleIgnoresSSHTerminals(t *testing.T) {
	rig := newSSHTestRig(t)
	recs := []ShellTerminalRecord{{HandleID: "shellterm-1", Title: "Terminal 1"}, {HandleID: "shellterm-ssh-0000000000000007", Title: "Terminal 7"}}
	if got := nextShellTerminalTitle(rig.svc.withoutSSHTerminals(recs)); got != "Terminal 2" {
		t.Fatalf("next title = %q, want Terminal 2", got)
	}
}

// Without the capability an SSH terminal answers rename and close exactly as an
// unknown handle does, and nothing is changed. With it they behave as today.
func TestRenameAndCloseSSHTerminalNeedTheCapability(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), requestscope.WithLAN(sshAccess())} {
		rig := newSSHTestRig(t)
		terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
		if err != nil {
			t.Fatalf("OpenSSHTerminal: %v", err)
		}
		ghost := "shellterm-ghost"

		_, unknownRename := rig.svc.RenameShellTerminal(ctx, ghost, "x")
		_, sshRename := rig.svc.RenameShellTerminal(ctx, terminal.HandleID, "x")
		assertSameAsUnknown(t, sshRename, unknownRename, ghost, terminal.HandleID)

		_, unknownEmpty := rig.svc.RenameShellTerminal(ctx, ghost, "  ")
		_, sshEmpty := rig.svc.RenameShellTerminal(ctx, terminal.HandleID, "  ")
		if a, b := apiErrorOf(t, sshEmpty), apiErrorOf(t, unknownEmpty); a.Code != b.Code || a.Message != b.Message {
			t.Fatalf("empty title: SSH %q/%q, unknown %q/%q", a.Code, a.Message, b.Code, b.Message)
		}

		unknownClose := rig.svc.CloseShellTerminal(ctx, ghost)
		sshClose := rig.svc.CloseShellTerminal(ctx, terminal.HandleID)
		assertSameAsUnknown(t, sshClose, unknownClose, ghost, terminal.HandleID)

		if rig.store.records[0].Title != "Build server" || len(rig.rt.destroyed) != 0 || len(rig.store.records) != 1 {
			t.Fatalf("a refused rename or close changed the SSH terminal: records %+v destroyed %v", rig.store.records, rig.rt.destroyed)
		}
		if !rig.svc.IsProtectedTerminal(terminal.HandleID) {
			t.Fatal("a refused close unprotected the SSH terminal")
		}

		renamed, err := rig.svc.RenameShellTerminal(sshAccess(), terminal.HandleID, "Build box")
		if err != nil || renamed.Title != "Build box" {
			t.Fatalf("rename with the capability = %+v, %v", renamed, err)
		}
		if err := rig.svc.CloseShellTerminal(sshAccess(), terminal.HandleID); err != nil {
			t.Fatalf("close with the capability: %v", err)
		}
		if len(rig.store.records) != 0 || len(rig.rt.destroyed) != 1 {
			t.Fatalf("close with the capability left records %+v destroyed %v", rig.store.records, rig.rt.destroyed)
		}
		rig.svc.sshMu.Lock()
		_, inSet := rig.svc.sshHandles[terminal.HandleID]
		rig.svc.sshMu.Unlock()
		if inSet {
			t.Fatal("a closed SSH terminal stayed in the set")
		}
	}
}

func assertSameAsUnknown(t *testing.T, sshErr, unknownErr error, ghost, handleID string) {
	t.Helper()
	got, want := apiErrorOf(t, sshErr), apiErrorOf(t, unknownErr)
	if got.Kind != want.Kind || got.Code != want.Code || got.Message != strings.ReplaceAll(want.Message, ghost, handleID) {
		t.Fatalf("SSH terminal answered %v %q %q, unknown handle %v %q %q", got.Kind, got.Code, got.Message, want.Kind, want.Code, want.Message)
	}
	if got.Kind != apierr.KindNotFound {
		t.Fatalf("kind = %v, want not found", got.Kind)
	}
}

// A runtime failure answers a fixed message; the runtime's words (which can
// carry the command line) reach neither the error nor the log.
func TestOpenSSHTerminalStartFailureHidesTheCause(t *testing.T) {
	rig := newSSHTestRig(t)
	rig.rt.createErr = errors.New("conpty: spawn pty-host: ssh.exe -l deploy -- build.example.com failed")
	_, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
	assertSSHRefusal(t, err, apierr.KindInternal, SSHTerminalStartFailedCode, "The SSH terminal could not be started. Try again.")
	if strings.Contains(err.Error(), "deploy") || strings.Contains(rig.logs.String(), "deploy") {
		t.Fatalf("the cause leaked: error %q log %q", err, rig.logs.String())
	}
	if !rig.svc.IsProtectedTerminal(testSSHFirstHandle) {
		t.Fatal("a failed open's handle is not protected")
	}
}

// Logs carry event names and handle ids only.
func TestSSHTerminalLogsCarryNoSecretsOrDestinations(t *testing.T) {
	rig := newSSHTestRig(t)
	terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample2Input())
	if err != nil {
		t.Fatalf("OpenSSHTerminal: %v", err)
	}
	_, _ = rig.svc.OpenSSHTerminal(context.Background(), sshSample2Input())
	_, _ = rig.svc.RenameShellTerminal(context.Background(), terminal.HandleID, "x")
	_, _ = rig.svc.RenameShellTerminal(sshAccess(), terminal.HandleID, "Renamed")
	_ = rig.svc.CloseShellTerminal(context.Background(), terminal.HandleID)
	_ = rig.svc.CloseShellTerminal(sshAccess(), terminal.HandleID)
	rig.rt.createErr = errors.New("spawn failed for 2001:db8::7")
	_, _ = rig.svc.OpenSSHTerminal(sshAccess(), sshSample2Input())

	logs := rig.logs.String()
	if !strings.Contains(logs, "ssh terminal opened") || !strings.Contains(logs, terminal.HandleID) {
		t.Fatalf("logs = %q, want the opened event with its handle id", logs)
	}
	for _, secret := range []string{testSSHToken, "2001:db8::7", "admin", testSSHKeyFile, "db_ed25519", "known-hosts", "known_hosts", "StrictHostKeyChecking", "ssh.exe", "auth-workspace", "Database"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs carry %q: %s", secret, logs)
		}
	}
}

// The token is kept only as a hash: nothing the service holds prints it.
func TestSSHTerminalServiceDoesNotKeepTheToken(t *testing.T) {
	rig := newSSHTestRig(t)
	dump := fmt.Sprintf("%+v %#v", rig.svc.ssh, rig.svc.ssh)
	if strings.Contains(dump, testSSHToken) || strings.Contains(dump, fmt.Sprintf("%x", testSSHToken)[:32]) {
		t.Fatalf("service state carries the token: %s", dump)
	}
}

// ssh gets an allow-listed environment and nothing else of the daemon's: no
// agent socket, no security-key library, no askpass program, no display, no
// AO setting, no token or key.
func TestOpenSSHTerminalGivesSSHAnAllowListedEnvironment(t *testing.T) {
	rig := newSSHTestRig(t)
	rig.environ = []string{
		`SystemRoot=C:\Windows`, `TEMP=C:\Temp`, `UserProfile=C:\Users\person`, `HOME=C:\Users\person`, "LANG=en_US.UTF-8",
		`SSH_SK_PROVIDER=C:\planted\sk.dll`, `SSH_AUTH_SOCK=\\.\pipe\planted`, `SSH_ASKPASS=C:\planted\askpass.exe`,
		"SSH_ASKPASS_REQUIRE=force", "DISPLAY=:0", "AO_X=1", "AO_SSH_TERMINAL_TOKEN=" + testSSHToken,
		"GITHUB_TOKEN=ghp_planted", "OPENAI_API_KEY=sk-planted", "ANTHROPIC_MODEL=x", `PATH=C:\planted`,
		"HTTPS_PROXY=http://proxy.invalid", "NODE_OPTIONS=--require planted", "=C:=C:\\",
	}
	if _, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input()); err != nil {
		t.Fatalf("OpenSSHTerminal: %v", err)
	}
	cfg := rig.rt.created[0]
	want := map[string]string{
		"SystemRoot": `C:\Windows`, "TEMP": `C:\Temp`, "UserProfile": `C:\Users\person`, "HOME": `C:\Users\person`,
		"LANG": "en_US.UTF-8", "SSH_ASKPASS_REQUIRE": "never",
	}
	if !cfg.EnvExact || !reflect.DeepEqual(cfg.Env, want) {
		t.Fatalf("env = %#v (exact %v), want exactly %#v", cfg.Env, cfg.EnvExact, want)
	}
}

// The runtime is asked for an exact environment only for an SSH terminal;
// every other terminal still adds to the daemon's.
func TestOnlySSHTerminalsAskForAnExactEnvironment(t *testing.T) {
	rig := newSSHTestRig(t)
	if _, err := rig.svc.OpenCommandTerminal(context.Background(), OpenCommandTerminalInput{Argv: []string{"pi"}, Title: "Log in to Pi"}); err != nil {
		t.Fatalf("OpenCommandTerminal: %v", err)
	}
	if rig.rt.created[0].EnvExact {
		t.Fatal("a command terminal asked for an exact environment")
	}
}

// Every SSH terminal starts in one shared folder: nothing under the data dir
// is named after a handle, and closing one leaves the folder.
func TestSSHTerminalsLeaveNoHandleNamedFolder(t *testing.T) {
	rig := newSSHTestRig(t)
	var handles []string
	for range 2 {
		terminal, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
		if err != nil {
			t.Fatalf("OpenSSHTerminal: %v", err)
		}
		handles = append(handles, terminal.HandleID)
		if want := filepath.Join(rig.svc.dataDir, sshWorkingDirectoryName); terminal.WorkingDir != want {
			t.Fatalf("working dir = %q, want %q", terminal.WorkingDir, want)
		}
	}
	assertNoHandleNamedEntry := func() {
		t.Helper()
		err := filepath.WalkDir(rig.svc.dataDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			for _, handle := range handles {
				if strings.Contains(strings.ToLower(entry.Name()), strings.ToLower(handle)) || strings.Contains(entry.Name(), sshHandlePrefix) {
					t.Fatalf("%s is named after an SSH handle", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertNoHandleNamedEntry()
	for _, handle := range handles {
		if err := rig.svc.CloseShellTerminal(sshAccess(), handle); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	assertNoHandleNamedEntry()
	if info, err := os.Stat(filepath.Join(rig.svc.dataDir, sshWorkingDirectoryName)); err != nil || !info.IsDir() {
		t.Fatalf("shared SSH folder after close: %v", err)
	}
}

// Off Windows the route is off whatever the configuration, and the token
// matches nothing (so it opens and attaches to nothing).
func TestSSHTerminalRouteIsWindowsOnly(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		rig := newSSHTestRig(t)
		rig.svc.goos = goos
		if rig.svc.SSHTerminalEnabled() || rig.svc.SSHTerminalTokenMatches(testSSHToken) {
			t.Fatalf("%s: route on", goos)
		}
		_, err := rig.svc.OpenSSHTerminal(sshAccess(), sshSample1Input())
		assertSSHRefusal(t, err, apierr.KindNotFound, SSHTerminalDisabledCode, "")
		if len(rig.rt.created) != 0 {
			t.Fatalf("%s: a runtime was started", goos)
		}
		// SSH terminals left from Windows days stay hidden and protected.
		if !rig.svc.IsProtectedTerminal("shellterm-ssh-0123456789abcdef") {
			t.Fatalf("%s: a minted SSH handle is not protected", goos)
		}
	}
	rig := newSSHTestRig(t)
	if !rig.svc.SSHTerminalEnabled() || !rig.svc.SSHTerminalTokenMatches(testSSHToken) {
		t.Fatal("windows: route off")
	}
}
