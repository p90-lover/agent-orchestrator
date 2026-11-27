package shellterm

// The in-app SSH terminal (Coding Tools: Connections > SSH hosts > Open
// terminal). The user asked for "a list of SSH hosts the user approves, an
// in-app SSH terminal for them" (2026-11-26).
//
// The daemon listens on loopback with no authentication, so any local process
// that knows its port (an agent among them) can call every route and attach to
// any terminal by id. An SSH session is the person's sign-in to another
// machine, so it is held to more than that:
//   - opening one needs a capability token only the app's main process holds
//     (AO_SSH_TERMINAL_TOKEN, removed from the daemon's environment at load), and
//     never answers a caller on the Connect Mobile LAN listener;
//   - the command line is built here from typed fields with the app's own rules
//     (ssh_rules.go); no caller-supplied option reaches ssh, which starts from
//     the kernel's Windows folder with an allow-listed environment in one shared
//     working folder whose name carries no handle;
//   - the route runs on Windows only (see sshTerminalPlatform);
//   - an SSH terminal is left out of the shared list, and renaming, closing or
//     attaching to one through /mux needs the token: without it the daemon
//     answers exactly as it does for a terminal that does not exist;
//   - logs carry event names and handle ids, never the command line, host,
//     user, key path, token or output.
//
// Residual risk, out of scope here: the ConPTY pty-host listens on an
// unauthenticated loopback port of its own.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
)

// SSHTerminalTokenHeader carries the SSH terminal capability on the app's own
// requests and on its /mux upgrade.
const SSHTerminalTokenHeader = "X-AO-SSH-Terminal-Token"

// Stable error codes of POST /api/v1/shell-terminals/ssh.
const (
	SSHTerminalLocalOnlyCode    = "SSH_TERMINAL_LOCAL_ONLY"
	SSHTerminalDisabledCode     = "SSH_TERMINAL_DISABLED"
	SSHTerminalForbiddenCode    = "SSH_TERMINAL_FORBIDDEN"
	SSHTerminalInvalidCode      = "SSH_TERMINAL_INVALID"
	SSHTerminalNoPinnedKeysCode = "SSH_TERMINAL_NO_PINNED_KEYS"
	SSHTerminalNoClientCode     = "SSH_TERMINAL_NO_CLIENT"
	SSHTerminalStartFailedCode  = "SSH_TERMINAL_START_FAILED"
)

// sshHandlePrefix names every SSH terminal's handle: the prefix and 16
// lower-case hex characters (see newSSHTerminalHandleID). The shape lets the
// daemon recognise an SSH terminal from its id alone: the in-memory set below
// empties when the daemon restarts, but the app's supervisor keeps the app run
// (and so this transient row and its live pty-host) across a daemon restart,
// and such a terminal must stay protected until the next app run reaps it.
const sshHandlePrefix = "shellterm-ssh-"

// sshHandlePattern is the exact minted shape, so that nothing else named with
// the prefix (a project whose session prefix is "shellterm-ssh", for one) is
// mistaken for an SSH terminal and locked out of its own terminals.
var sshHandlePattern = regexp.MustCompile(`^shellterm-ssh-[0-9a-f]{16}$`)

// minSSHTerminalTokenLength matches config: the app sends 64 hex characters.
const minSSHTerminalTokenLength = 32

// sshWorkingDirectoryName is the one folder, under the data dir, every SSH
// terminal starts in. ssh reads nothing from its working folder, and a folder
// per terminal would carry the handle id into a name the folder browser
// (GET /api/v1/fs/dirs) lists to every local caller. It is created on demand
// and never removed with a terminal.
const sshWorkingDirectoryName = "ssh-terminal"

// sshTerminalPlatform is the only platform the SSH route runs on. Coding Tools
// ships on Windows only, and only there does removing the token from the
// environment (config.Load) take it out of what other processes can read: on
// Linux and macOS /proc/<pid>/environ and `ps eww` keep the start-up copy. So
// the route is off everywhere else (404 SSH_TERMINAL_DISABLED), whatever the
// token. Service.goos holds the running platform; tests set it.
const sshTerminalPlatform = "windows"

// sshChildEnvironmentNames is what ssh may see of the daemon's environment:
// the app's own probe allow-list (childEnvironment in the app's
// ssh-connections.cjs), matched without regard to case. Nothing else passes:
// no SSH_ASKPASS, SSH_SK_PROVIDER (a library ssh loads), SSH_AUTH_SOCK,
// DISPLAY, AO_* or any token or key. The pty layer adds TERM and COLORTERM.
var sshChildEnvironmentNames = map[string]bool{
	"systemroot": true, "systemdrive": true, "windir": true, "temp": true, "tmp": true, "userprofile": true,
	"homedrive": true, "homepath": true, "username": true, "userdomain": true, "computername": true,
	"programdata": true, "allusersprofile": true, "localappdata": true, "appdata": true,
	"home": true, "user": true, "logname": true, "lang": true, "lc_all": true, "tz": true,
}

// sshChildEnvironment builds ssh's whole environment from environ (the
// daemon's, as os.Environ lists it): the allow-listed names, and
// SSH_ASKPASS_REQUIRE=never so ssh never starts an askpass program. Every
// prompt is answered by the person in the terminal.
func sshChildEnvironment(environ []string) map[string]string {
	env := map[string]string{}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || !sshChildEnvironmentNames[strings.ToLower(name)] {
			continue
		}
		env[name] = value
	}
	env["SSH_ASKPASS_REQUIRE"] = "never"
	return env
}

// SSHTerminalSettings turns the SSH terminal route on. Both fields are needed;
// either one empty leaves the route off.
type SSHTerminalSettings struct {
	// KnownHostsFile is the absolute path of the pinned known_hosts file the app
	// writes from the hosts the person approved.
	KnownHostsFile string
	// Token is the capability the app's main process presents.
	Token string
}

// Option configures a Service at construction.
type Option func(*Service)

// WithSSHTerminal turns the SSH terminal route on. Only a hash of the token is
// kept, so the service holds nothing that could be printed back.
func WithSSHTerminal(settings SSHTerminalSettings) Option {
	return func(s *Service) {
		if settings.KnownHostsFile == "" || len(settings.Token) < minSSHTerminalTokenLength {
			s.ssh = sshTerminalState{}
			return
		}
		s.ssh = sshTerminalState{
			enabled:        true,
			knownHostsFile: settings.KnownHostsFile,
			tokenHash:      sha256.Sum256([]byte(settings.Token)),
		}
	}
}

type sshTerminalState struct {
	enabled        bool
	knownHostsFile string
	tokenHash      [sha256.Size]byte
}

type sshAccessKey struct{}

// WithSSHTerminalAccess marks a request context as one that presented the SSH
// terminal capability on the loopback listener. The HTTP layer adds it only
// after SSHTerminalTokenMatches; nothing else may.
func WithSSHTerminalAccess(ctx context.Context) context.Context {
	return context.WithValue(ctx, sshAccessKey{}, true)
}

// HasSSHTerminalAccess reports whether WithSSHTerminalAccess marked ctx. A LAN
// request never has access, whatever it carries.
func HasSSHTerminalAccess(ctx context.Context) bool {
	marked, _ := ctx.Value(sshAccessKey{}).(bool)
	return marked && !requestscope.IsLAN(ctx)
}

// SSHTerminalEnabled reports whether the route is on: configured (a
// known_hosts path and a token) and on Windows (see sshTerminalGOOS).
func (s *Service) SSHTerminalEnabled() bool {
	return s.ssh.enabled && s.goos == sshTerminalPlatform
}

// sshWindows reports whether the Windows path rules apply. The route runs only
// on Windows; the POSIX rules remain for the tests that port them.
func (s *Service) sshWindows() bool { return s.goos == "windows" }

// SSHTerminalTokenMatches reports whether presented is this run's capability.
// Both sides are hashed before the constant-time compare, so neither the
// token's value nor its length is learnt from timing. With the route off no
// value matches, so the token opens nothing (and attaches to nothing) there.
func (s *Service) SSHTerminalTokenMatches(presented string) bool {
	if !s.SSHTerminalEnabled() || presented == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(sum[:], s.ssh.tokenHash[:]) == 1
}

// IsProtectedTerminal reports whether id names an SSH terminal: one of the
// minted shape (in any wrapped form a runtime adapter gives it, such as
// ptyhost-v1:<id>), or one this daemon recorded as SSH when it opened it. It
// satisfies terminal.ProtectedTerminals for the /mux gate.
func (s *Service) IsProtectedTerminal(id string) bool {
	if isSSHHandleID(id) {
		return true
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	_, ok := s.sshHandles[id]
	return ok
}

// isSSHHandleID matches the minted shape on the id's last ':'-separated part,
// trimmed and lower-cased: a spelling no runtime resolves is refused too,
// never let through.
func isSSHHandleID(id string) bool {
	id = strings.TrimSpace(id)
	if i := strings.LastIndexByte(id, ':'); i >= 0 {
		id = id[i+1:]
	}
	return sshHandlePattern.MatchString(strings.ToLower(strings.TrimSpace(id)))
}

func (s *Service) protectSSHHandle(id string) {
	if id == "" {
		return
	}
	s.sshMu.Lock()
	s.sshHandles[id] = struct{}{}
	s.sshMu.Unlock()
}

func (s *Service) forgetSSHHandle(id string) {
	s.sshMu.Lock()
	delete(s.sshHandles, id)
	s.sshMu.Unlock()
}

// withoutSSHTerminals drops SSH terminals from records a caller without the
// capability will see the effect of (the shared list, the next tab's number).
func (s *Service) withoutSSHTerminals(recs []ShellTerminalRecord) []ShellTerminalRecord {
	out := make([]ShellTerminalRecord, 0, len(recs))
	for _, rec := range recs {
		if !s.IsProtectedTerminal(rec.HandleID) {
			out = append(out, rec)
		}
	}
	return out
}

func newSSHTerminalHandleID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return sshHandlePrefix + hex.EncodeToString(buf), nil
}

func (s *Service) isRegularFile(path string) bool {
	info, err := s.statFile(path)
	return err == nil && info.Mode().IsRegular()
}

// sshWorkingDirectory is the one shared folder SSH terminals start in, created
// on demand (see sshWorkingDirectoryName). No name under it, or of it, carries
// a handle id.
func (s *Service) sshWorkingDirectory() (string, error) {
	if s.dataDir == "" {
		return "", os.ErrNotExist
	}
	dir := filepath.Join(s.dataDir, sshWorkingDirectoryName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func sshTerminalInvalid(message string) error {
	return apierr.Invalid(SSHTerminalInvalidCode, message, nil)
}

// NewSSHTerminalLocalOnlyError and the constructors below are the route's refusals,
// shared by the controller (which answers the first three before it reads the
// body) and the service (which checks them again).
func NewSSHTerminalLocalOnlyError() error {
	return apierr.Forbidden(SSHTerminalLocalOnlyCode, "SSH terminals can be opened only on this computer.")
}

// NewSSHTerminalDisabledError answers when the route is not configured, or the
// daemon is not on Windows.
func NewSSHTerminalDisabledError() error {
	return apierr.NotFound(SSHTerminalDisabledCode, "SSH terminals are not available in this daemon.")
}

// NewSSHTerminalForbiddenError answers a missing or wrong capability, with no detail
// about which.
func NewSSHTerminalForbiddenError() error {
	return apierr.Forbidden(SSHTerminalForbiddenCode, "This request is not allowed.")
}

// NewSSHTerminalInvalidError answers a request body that cannot be read as the
// route's fields.
func NewSSHTerminalInvalidError(message string) error { return sshTerminalInvalid(message) }

// validateSSHTerminalInput checks every field in a fixed order (host, port,
// user, key file, title, client) and answers the first one that is wrong.
func (s *Service) validateSSHTerminalInput(in OpenSSHTerminalInput) (sshTerminalTarget, string, error) {
	host, problem := validateSSHHost(in.Host)
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	// Only the stored form is taken: ssh names the pinned key by it, and two
	// spellings of one server must not reach it as two hosts.
	if host != in.Host {
		return sshTerminalTarget{}, "", sshTerminalInvalid("Host: use the stored lower-case host name (an IPv6 address in its short form).")
	}
	port, problem := validateSSHPort(in.Port)
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	user, problem := validateSSHUser(in.User)
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	identityFile, problem := validateSSHIdentityFile(in.IdentityFile, s.sshWindows())
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	title, problem := validateSSHTitle(in.Title)
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	client, problem := validateSSHClient(in.Client)
	if problem != "" {
		return sshTerminalTarget{}, "", sshTerminalInvalid(problem)
	}
	// A key file that is gone is not skipped by ssh: it warns, drops -i and
	// offers the person's default keys to the host, IdentitiesOnly or not (the
	// app's review, 2026-11-27, ssh -G on the target machine).
	if identityFile != "" && !s.isRegularFile(identityFile) {
		return sshTerminalTarget{}, "", sshTerminalInvalid("Key file: no file was found at that path, so the terminal was not opened. Put the key file back, or remove this host and add it again.")
	}
	return sshTerminalTarget{host: host, port: port, user: user, identityFile: identityFile, client: client}, title, nil
}

// OpenSSHTerminal opens an ssh session to one host the person approved, in a
// transient command terminal (reaped on the next app run) that starts in the
// shared SSH working folder with an allow-listed environment. The checks run
// in a fixed order: LAN caller, route off (also: not Windows), capability, the
// request's fields, the pinned keys file, the ssh program.
//
// The handle is protected before the runtime exists: its id carries the SSH
// prefix from the moment it is minted, and it is in the set before Create, so
// no list, rename, close or /mux open can reach it in the window between the
// pty-host starting and its row being written.
func (s *Service) OpenSSHTerminal(ctx context.Context, in OpenSSHTerminalInput) (ShellTerminal, error) {
	if requestscope.IsLAN(ctx) {
		return ShellTerminal{}, NewSSHTerminalLocalOnlyError()
	}
	if !s.SSHTerminalEnabled() {
		return ShellTerminal{}, NewSSHTerminalDisabledError()
	}
	if !HasSSHTerminalAccess(ctx) {
		return ShellTerminal{}, NewSSHTerminalForbiddenError()
	}
	target, title, err := s.validateSSHTerminalInput(in)
	if err != nil {
		return ShellTerminal{}, err
	}
	if _, ok := sshOptionPath(s.ssh.knownHostsFile, s.sshWindows()); !ok {
		return ShellTerminal{}, apierr.Conflict(SSHTerminalNoPinnedKeysCode,
			"The pinned keys file is in a folder ssh cannot be given, so the terminal was not opened.", nil)
	}
	if !s.isRegularFile(s.ssh.knownHostsFile) {
		return ShellTerminal{}, apierr.Conflict(SSHTerminalNoPinnedKeysCode,
			"No approved host key was found on this computer, so the terminal was not opened. Check the host's key on the SSH hosts page, then try again.", nil)
	}
	program, ok := sshProgramPath(s.sshWindows(), s.windowsDirectory)
	if !ok || !s.isRegularFile(program) {
		return ShellTerminal{}, apierr.Conflict(SSHTerminalNoClientCode,
			"OpenSSH was not found on this computer, so the terminal was not opened.", nil)
	}
	argv, ok := sshTerminalArgv(target, s.ssh.knownHostsFile, s.sshWindows())
	if !ok {
		return ShellTerminal{}, apierr.Conflict(SSHTerminalNoPinnedKeysCode,
			"The pinned keys file is in a folder ssh cannot be given, so the terminal was not opened.", nil)
	}
	workingDir, err := s.sshWorkingDirectory()
	if err != nil {
		s.log.Warn("ssh terminal open failed", "stage", "working_dir")
		return ShellTerminal{}, sshTerminalStartFailed()
	}
	command := OpenCommandTerminalInput{
		Argv: append([]string{program}, argv...),
		// The whole environment, not additions to the daemon's (see
		// sshChildEnvironment).
		Env:        sshChildEnvironment(s.environ()),
		WorkingDir: workingDir,
		Title:      title,
	}
	if err := validateOpenCommandTerminalInput(command); err != nil {
		return ShellTerminal{}, err
	}
	handleID, err := s.newSSHHandleID()
	if err != nil {
		s.log.Warn("ssh terminal open failed", "stage", "handle_id")
		return ShellTerminal{}, sshTerminalStartFailed()
	}
	s.protectSSHHandle(handleID)
	terminal, err := s.openCommandTerminal(ctx, command, commandTerminalOptions{
		handleID: handleID,
		// A runtime adapter may wrap the id it was given; the id it answers is
		// protected too, before the row that lists it is written.
		onCreated: s.protectSSHHandle,
		quietLog:  true,
		envExact:  true,
	})
	if err != nil {
		// The cause stays out of the error and the log: a runtime's message can
		// carry the command line. The set keeps the id; a failed open leaves
		// nothing reachable behind it either way.
		s.log.Warn("ssh terminal open failed", "handleId", handleID)
		return ShellTerminal{}, sshTerminalStartFailed()
	}
	s.log.Info("ssh terminal opened", "handleId", terminal.HandleID)
	return terminal, nil
}

func sshTerminalStartFailed() error {
	return apierr.Internal(SSHTerminalStartFailedCode, "The SSH terminal could not be started. Try again.")
}

func shellTerminalNotFound(handleID string) error {
	return apierr.NotFound("SHELL_TERMINAL_NOT_FOUND", "No such shell terminal: "+handleID)
}

// osStat is the default file check; tests replace Service.statFile.
var osStat = os.Stat
