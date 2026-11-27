package shellterm

// The in-app SSH terminal's input rules and command line, ported from the
// Coding Tools app's SSH hosts store (desktop-electron/electron/
// ssh-connections.cjs: validateHost, validatePort, validateUser,
// validateIdentityFile, validateLabel, optionPath, hostKeyOptions,
// LOCKED_OPTIONS and terminalArgv). The app is the authority for these rules;
// this copy exists because the daemon has no authentication of its own, so it
// builds the ssh command line itself from typed fields and accepts nothing
// looser than the app does. The ssh program's folder is the one exception: it
// comes from the kernel here, not from SystemRoot (see sshProgramPath).
//
// JavaScript semantics are reproduced where they decide a result: a length is
// counted in UTF-16 code units, `\s` and String.prototype.trim use ECMAScript
// white space, `\p{C}` includes unassigned code points, and paths are
// normalised as Node's path.win32 / path.posix normalize do. The messages are
// the app's own words, so the hosts page can show them as they are.

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const sshDefaultPort = 22

// maxSSHTitleRunes bounds the terminal title. The app sends the host's label
// trimmed to 80 characters; the shell terminal title bound is the same 80.
const maxSSHTitleRunes = maxShellTerminalTitleLen

// maxSSHClientVersionPart bounds client.major and client.minor: the app reads
// them from `ssh -V` as one to three digits (parseClientVersion).
const maxSSHClientVersionPart = 999

var (
	sshHostPortPattern     = regexp.MustCompile(`^[^:]+:[0-9]+$`)
	sshHexColonPattern     = regexp.MustCompile(`^[0-9A-Fa-f:]+$`)
	sshDigitsDotsPattern   = regexp.MustCompile(`^[0-9.]+$`)
	sshDigitsPattern       = regexp.MustCompile(`^[0-9]+$`)
	sshHexLabelPattern     = regexp.MustCompile(`^0x[0-9a-f]*$`)
	sshUserPattern         = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sshDriveRootPattern    = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	sshWindowsPubPattern   = regexp.MustCompile(`(?i)\.pub[. ]*$`)
	sshPosixPubPattern     = regexp.MustCompile(`(?i)\.pub$`)
	sshNodeIPv4Pattern     = regexp.MustCompile(`^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])$`)
	sshHostCharacterFilter = func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == ':' || r == '-')
	}
)

// sshLockedOptions are set on every command line after the host-key options:
// nothing is forwarded, nothing local runs, no connection is shared, and no
// agent is asked for keys (the app's LOCKED_OPTIONS, in its order).
var sshLockedOptions = [][2]string{
	{"UpdateHostKeys", "no"}, {"VerifyHostKeyDNS", "no"},
	{"ForwardAgent", "no"}, {"ForwardX11", "no"}, {"ForwardX11Trusted", "no"}, {"ClearAllForwardings", "yes"}, {"Tunnel", "no"},
	{"PermitLocalCommand", "no"}, {"ProxyCommand", "none"}, {"ControlMaster", "no"}, {"ControlPath", "none"},
	{"Compression", "no"}, {"IdentityAgent", "none"}, {"AddKeysToAgent", "no"},
}

// jsLength is String.prototype.length: UTF-16 code units.
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// isJSSpace is ECMAScript white space and line terminators: what `\s`
// matches and what trim removes.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func isASCIIControl(r rune) bool { return r < 0x20 || r == 0x7f }

func containsASCIIControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// isJSInvisible is the app's label rule /[\p{C}\p{Zl}\p{Zp}]/u. \p{C} in
// JavaScript includes unassigned code points (Cn); Go's unicode.C does not, so
// a rune in no general category is counted as well.
func isJSInvisible(r rune) bool {
	if r == 0x2028 || r == 0x2029 || unicode.Is(unicode.C, r) {
		return true
	}
	return !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z)
}

func describeSSHCharacter(c string) string {
	switch c {
	case `"`:
		return "A double quote"
	case "'":
		return "A single quote"
	case "`":
		return "A backtick"
	}
	return `"` + c + `"`
}

// validateSSHHost is the app's validateHost: a host name, an IPv4 address or an
// IPv6 address, returned in the form ssh writes (lower case; an IPv6 address in
// its short form). The second result is the problem, or "".
func validateSSHHost(input string) (string, string) {
	if input == "" {
		return "", "Host: enter a host name or an IP address."
	}
	if jsLength(input) > 253 {
		return "", "Host: a host name can be at most 253 characters."
	}
	if strings.IndexFunc(input, func(r rune) bool { return isJSSpace(r) || isASCIIControl(r) }) >= 0 {
		return "", "Host: a host name cannot contain spaces, tabs or line breaks. Remove them and try again."
	}
	if strings.IndexFunc(input, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return "", "Host: use plain ASCII letters and digits only. For an international name, enter its xn-- form."
	}
	if strings.Contains(input, "://") {
		return "", "Host: enter the host name only, not an ssh:// address. The user and the port have their own fields."
	}
	if strings.Contains(input, "@") {
		return "", "Host: enter the host name only. The user has its own field."
	}
	if strings.HasPrefix(input, "-") {
		return "", "Host: a host name cannot start with a hyphen."
	}
	if strings.ContainsAny(input, "[]") {
		return "", "Host: enter an IPv6 address without square brackets. The port has its own field."
	}
	if strings.Contains(input, "%") && strings.Contains(input, ":") {
		return "", "Host: an IPv6 address with a zone (the part after %) is not supported."
	}
	// Every character left is printable ASCII, so one byte is one character.
	if i := strings.IndexFunc(input, sshHostCharacterFilter); i >= 0 {
		return "", "Host: " + describeSSHCharacter(input[i:i+1]) + " is not allowed in a host name. Use letters, digits, dots and hyphens."
	}
	if strings.Contains(input, ":") {
		if sshHostPortPattern.MatchString(input) {
			return "", "Host: enter the host name only. The port has its own field."
		}
		addr, err := netip.ParseAddr(input)
		if !sshHexColonPattern.MatchString(input) || err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", "Host: this is not a valid IPv6 address. Use hex digits and colons only, like 2001:db8::1."
		}
		if sshEmbedsIPv4(ipv6Groups(addr)) {
			return "", "Host: this IPv6 address has an IPv4 address inside it, which ssh writes another way. Enter the server's IPv4 address, or another IPv6 address of it."
		}
		// The short form ssh writes before it looks up or saves a key: lower
		// case, no leading zeros, the first longest run of zero groups as "::".
		// netip writes exactly that for every address left here (the one form
		// it writes differently, an IPv4-mapped address, was refused above).
		return addr.String(), ""
	}
	host := strings.ToLower(input)
	last := host[strings.LastIndex(host, ".")+1:]
	// A name that ends in a number is an address to the resolver ("127.1",
	// "0x7f.1" and a bare "2130706433" all reach 127.0.0.1 on some systems), so
	// it must be a full dotted address.
	if sshDigitsDotsPattern.MatchString(host) || sshDigitsPattern.MatchString(last) || sshHexLabelPattern.MatchString(last) {
		if !sshNodeIPv4Pattern.MatchString(host) {
			return "", "Host: this looks like an IP address but is not a valid one. Use four numbers from 0 to 255, like 192.0.2.10."
		}
		return host, ""
	}
	if !isLowerAlnum(host[0]) || !isLowerAlnum(host[len(host)-1]) {
		return "", "Host: a host name starts and ends with a letter or a digit."
	}
	if strings.Contains(host, "..") {
		return "", "Host: a host name cannot have two dots in a row."
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) > 63 {
			return "", "Host: each part of a host name (between dots) can be at most 63 characters."
		}
	}
	return host, ""
}

func isLowerAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

func ipv6Groups(addr netip.Addr) [8]uint16 {
	b := addr.As16()
	var groups [8]uint16
	for i := range groups {
		groups[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return groups
}

// sshEmbedsIPv4 reports whether ssh writes this address with an IPv4 address
// inside it: mapped, compatible, translated and ISATAP forms (the app's
// embedsIpv4, checked there with `ssh -G` on OpenSSH_for_Windows_9.5p2).
func sshEmbedsIPv4(g [8]uint16) bool {
	zero := func(from, to int) bool {
		for i := from; i < to; i++ {
			if g[i] != 0 {
				return false
			}
		}
		return true
	}
	if zero(0, 5) && (g[5] == 0xffff || (g[5] == 0 && g[6] != 0)) {
		return true
	}
	if zero(0, 4) && g[4] == 0xffff && g[5] == 0 {
		return true
	}
	return g[5] == 0x5efe && g[4]&0xfdff == 0
}

// validateSSHPort is the app's validatePort for a JSON number: nothing given
// means 22. A port sent as a JSON string is refused before this (the route
// takes numbers only), and so is a number with a fraction or an exponent.
func validateSSHPort(port *int) (int, string) {
	if port == nil {
		return sshDefaultPort, ""
	}
	// The app reads String(port) against /^[0-9]{1,5}$/: a negative number or
	// one of six digits or more fails that test, a five-digit one the range.
	if *port < 0 || *port > 99999 {
		return 0, "Port: use digits only, from 1 to 65535. Leave it empty for 22."
	}
	if *port < 1 || *port > 65535 {
		return 0, "Port: use a number from 1 to 65535. Leave it empty for 22."
	}
	return *port, ""
}

// validateSSHUser is the app's validateUser.
func validateSSHUser(input string) (string, string) {
	if input == "" {
		return "", "User: enter the user name you sign in with on the server."
	}
	if jsLength(input) > 32 {
		return "", "User: a user name can be at most 32 characters."
	}
	if strings.IndexFunc(input, func(r rune) bool { return isJSSpace(r) || isASCIIControl(r) }) >= 0 {
		return "", "User: a user name cannot contain spaces, tabs or line breaks."
	}
	if strings.Contains(input, "@") {
		return "", "User: enter the user name only, without @ and the host. The host has its own field."
	}
	if strings.Contains(input, `\`) {
		return "", `User: Windows domain logins (DOMAIN\name) are not supported. Enter the user name only.`
	}
	if strings.HasPrefix(input, "-") {
		return "", "User: a user name cannot start with a hyphen."
	}
	if !sshUserPattern.MatchString(input) {
		return "", "User: use letters, digits, dots, underscores and hyphens only, starting with a letter, a digit or an underscore."
	}
	return input, ""
}

// sshIdentityFileProblem is the app's identityFileProblem.
func sshIdentityFileProblem(text string, windows bool) string {
	if jsLength(text) > 1024 {
		return "Key file: the path is too long."
	}
	if containsASCIIControl(text) {
		return "Key file: the path cannot contain line breaks or control characters."
	}
	if strings.Contains(text, `"`) {
		return "Key file: the path cannot contain double quotes."
	}
	// ssh expands % tokens and ${NAME} in a key path, so the file it opened
	// would not be the one named.
	if strings.Contains(text, "%") || strings.Contains(text, "${") {
		return "Key file: the path cannot contain % or ${ (ssh would replace them). Move or rename the file."
	}
	if windows && !sshDriveRootPattern.MatchString(text) {
		return `Key file: enter the full path, starting with a drive letter (for example C:\Users\you\.ssh\id_ed25519).`
	}
	if !windows && !strings.HasPrefix(text, "/") {
		return "Key file: enter the full path, starting with /."
	}
	// Windows drops trailing dots and spaces from a name: "id.pub." and
	// "id.pub " open id.pub.
	if windows && sshWindowsPubPattern.MatchString(text) || !windows && sshPosixPubPattern.MatchString(text) {
		return "Key file: choose the private key, not the .pub file next to it."
	}
	return ""
}

// validateSSHIdentityFile is the app's validateIdentityFile: "" means no key
// file. The rules are judged again on the path as it is kept: a path that
// passed as typed can name the .pub file once normalised.
func validateSSHIdentityFile(input string, windows bool) (string, string) {
	if input == "" {
		return "", ""
	}
	if problem := sshIdentityFileProblem(input, windows); problem != "" {
		return "", problem
	}
	file := posixNormalize(input)
	if windows {
		file = win32Normalize(input)
	}
	if problem := sshIdentityFileProblem(file, windows); problem != "" {
		return "", problem
	}
	return file, ""
}

// validateSSHTitle is the app's validateLabel with the terminal title's bound
// (80 characters): trimmed, not empty, nothing invisible.
func validateSSHTitle(input string) (string, string) {
	title := strings.TrimFunc(input, isJSSpace)
	if title == "" {
		return "", "Title: enter a name for this terminal."
	}
	if utf8.RuneCountInString(title) > maxSSHTitleRunes {
		return "", "Title: use at most 80 characters."
	}
	if strings.IndexFunc(title, isJSInvisible) >= 0 {
		return "", "Title: a title cannot contain line breaks or invisible characters."
	}
	return title, ""
}

// sshClientVersion is the OpenSSH version the app read from `ssh -V`. nil
// counts as current, as the app's clientAtLeast does.
type sshClientVersion struct{ major, minor int }

func validateSSHClient(in *SSHClientVersionInput) (*sshClientVersion, string) {
	if in == nil {
		return nil, ""
	}
	valid := func(part *int) bool { return part != nil && *part >= 0 && *part <= maxSSHClientVersionPart }
	if !valid(in.Major) || !valid(in.Minor) {
		return nil, "Client: send the OpenSSH version as two whole numbers from 0 to 999 (major and minor)."
	}
	return &sshClientVersion{major: *in.Major, minor: *in.Minor}, ""
}

// sshClientAtLeast is the app's clientAtLeast.
func sshClientAtLeast(client *sshClientVersion, major, minor int) bool {
	if client == nil {
		return true
	}
	return client.major > major || client.major == major && client.minor >= minor
}

// sshOptionPath is the app's optionPath: a file path as an ssh option value.
// The value is read as a config line, so a path with a space is split unless
// it sits in double quotes inside the value; forward slashes keep a backslash
// from being read as an escape, and %% keeps a percent sign from being read as
// a token. false: ssh cannot be given this path.
func sshOptionPath(file string, windows bool) (string, bool) {
	absolute := strings.HasPrefix(file, "/")
	if windows {
		absolute = win32IsAbsolute(file)
	}
	if !absolute || containsASCIIControl(file) || strings.Contains(file, `"`) || strings.Contains(file, "${") ||
		(!windows && strings.Contains(file, `\`)) {
		return "", false
	}
	if windows {
		file = strings.ReplaceAll(file, `\`, "/")
	}
	return `"` + strings.ReplaceAll(file, "%", "%%") + `"`, true
}

// sshProgramPath is ssh by absolute path, never looked up on PATH or in the
// current folder, so a planted ssh there does not run. On Windows the folder
// is the one the kernel reports (GetSystemWindowsDirectory), not %SystemRoot%:
// the daemon's environment is inherited from whatever started the app, so no
// variable decides which program runs. This differs on purpose from the app's
// sshProgram, which reads SystemRoot under rules that refuse a planted value.
// false: the folder could not be read or is not a plain drive path.
func sshProgramPath(windows bool, windowsDirectory func() (string, error)) (string, bool) {
	if !windows {
		return "/usr/bin/ssh", true
	}
	dir, err := windowsDirectory()
	if err != nil || !sshDriveRootPattern.MatchString(dir) || strings.ContainsAny(dir, `"<>|?*`) || containsASCIIControl(dir) {
		return "", false
	}
	program := win32Normalize(strings.TrimRight(dir, `\/`) + `\System32\OpenSSH\ssh.exe`)
	return program, program != ""
}

// sshTerminalTarget is one validated request: every field is in the form the
// command line takes it.
type sshTerminalTarget struct {
	host         string
	port         int
	user         string
	identityFile string
	client       *sshClientVersion
}

// sshTerminalArgv is the app's terminalArgv (after the program): the server
// must show the pinned key, the person answers every prompt in the terminal,
// and nothing follows the host. The client version only adds CheckHostIP=no
// (before 8.5) and EnableEscapeCommandline=no (9.2 and later); a client that
// lies about it can only add a harmless option or make ssh stop, never weaken
// a check. false: the pinned file's path cannot be given to ssh.
func sshTerminalArgv(target sshTerminalTarget, knownHostsFile string, windows bool) ([]string, bool) {
	userKnownHosts, ok := sshOptionPath(knownHostsFile, windows)
	if !ok {
		return nil, false
	}
	options := [][2]string{
		{"StrictHostKeyChecking", "yes"},
		{"UserKnownHostsFile", userKnownHosts},
		{"GlobalKnownHostsFile", "none"},
	}
	// Before OpenSSH 8.5 CheckHostIP was on unless turned off, and a terminal
	// would add the address to the pinned file.
	if !sshClientAtLeast(target.client, 8, 5) {
		options = append(options, [2]string{"CheckHostIP", "no"})
	}
	options = append(options, sshLockedOptions...)
	options = append(options, [2]string{"EscapeChar", "none"})
	// Added in OpenSSH 9.2; an older client stops at an option it does not know.
	if sshClientAtLeast(target.client, 9, 2) {
		options = append(options, [2]string{"EnableEscapeCommandline", "no"})
	}
	options = append(options,
		[2]string{"ConnectTimeout", "15"}, [2]string{"ConnectionAttempts", "1"},
		[2]string{"ServerAliveInterval", "30"}, [2]string{"ServerAliveCountMax", "3"},
		[2]string{"BatchMode", "no"},
	)
	if target.identityFile != "" {
		options = append(options, [2]string{"IdentitiesOnly", "yes"})
	}
	argv := []string{"-F", "none"}
	for _, option := range options {
		argv = append(argv, "-o", option[0]+"="+option[1])
	}
	if target.identityFile != "" {
		argv = append(argv, "-i", target.identityFile)
	}
	return append(argv, "-t", "-p", strconv.Itoa(target.port), "-l", target.user, "--", target.host), true
}

// win32IsAbsolute is Node's path.win32.isAbsolute.
func win32IsAbsolute(p string) bool {
	if p == "" {
		return false
	}
	if isWin32Separator(uint16(p[0])) {
		return true
	}
	return len(p) > 2 && isDriveLetter(uint16(p[0])) && p[1] == ':' && isWin32Separator(uint16(p[2]))
}

func isWin32Separator(c uint16) bool { return c == '\\' || c == '/' }

func isPosixSeparator(c uint16) bool { return c == '/' }

func isDriveLetter(c uint16) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// win32Normalize is Node's path.win32.normalize for a path that starts with a
// drive letter and a colon, the only form these rules pass to it (the
// identity-file and SystemRoot checks require it first). Any other path
// answers "", which fails every rule that compares against it.
func win32Normalize(p string) string {
	path := utf16.Encode([]rune(p))
	n := len(path)
	if n < 2 || !isDriveLetter(path[0]) || path[1] != ':' {
		return ""
	}
	device := path[:2]
	rootEnd := 2
	absolute := false
	if n > 2 && isWin32Separator(path[2]) {
		// A separator after the drive name marks an absolute path.
		absolute = true
		rootEnd = 3
	}
	var tail []uint16
	if rootEnd < n {
		tail = normalizeJSPathString(path[rootEnd:], !absolute, '\\', isWin32Separator)
	}
	if len(tail) == 0 && !absolute {
		tail = []uint16{'.'}
	}
	if len(tail) > 0 && isWin32Separator(path[n-1]) {
		tail = append(tail, '\\')
	}
	out := append([]uint16{}, device...)
	if absolute {
		out = append(out, '\\')
	}
	return string(utf16.Decode(append(out, tail...)))
}

// posixNormalize is Node's path.posix.normalize.
func posixNormalize(p string) string {
	path := utf16.Encode([]rune(p))
	n := len(path)
	if n == 0 {
		return "."
	}
	absolute := path[0] == '/'
	trailing := path[n-1] == '/'
	res := normalizeJSPathString(path, !absolute, '/', isPosixSeparator)
	if len(res) == 0 {
		if absolute {
			return "/"
		}
		if trailing {
			return "./"
		}
		return "."
	}
	if trailing {
		res = append(res, '/')
	}
	if absolute {
		return "/" + string(utf16.Decode(res))
	}
	return string(utf16.Decode(res))
}

// normalizeJSPathString is Node's normalizeString (lib/path.js): it resolves
// "." and ".." segments and doubled separators, on UTF-16 code units as Node
// does.
func normalizeJSPathString(path []uint16, allowAboveRoot bool, separator uint16, isSeparator func(uint16) bool) []uint16 {
	res := []uint16{}
	lastSegmentLength := 0
	lastSlash := -1
	dots := 0
	var code uint16
	lastIndex := func(s []uint16, c uint16) int {
		for i := len(s) - 1; i >= 0; i-- {
			if s[i] == c {
				return i
			}
		}
		return -1
	}
	for i := 0; i <= len(path); i++ {
		if i < len(path) {
			code = path[i]
		} else if isSeparator(code) {
			break
		} else {
			code = '/'
		}
		if isSeparator(code) {
			switch {
			case lastSlash == i-1 || dots == 1:
				// NOOP
			case dots == 2:
				if len(res) < 2 || lastSegmentLength != 2 || res[len(res)-1] != '.' || res[len(res)-2] != '.' {
					if len(res) > 2 {
						if slash := lastIndex(res, separator); slash == -1 {
							res = res[:0]
							lastSegmentLength = 0
						} else {
							res = res[:slash]
							lastSegmentLength = len(res) - 1 - lastIndex(res, separator)
						}
						lastSlash = i
						dots = 0
						continue
					} else if len(res) != 0 {
						res = res[:0]
						lastSegmentLength = 0
						lastSlash = i
						dots = 0
						continue
					}
				}
				if allowAboveRoot {
					if len(res) > 0 {
						res = append(res, separator, '.', '.')
					} else {
						res = append(res, '.', '.')
					}
					lastSegmentLength = 2
				}
			default:
				if len(res) > 0 {
					res = append(res, separator)
				}
				res = append(res, path[lastSlash+1:i]...)
				lastSegmentLength = i - lastSlash - 1
			}
			lastSlash = i
			dots = 0
		} else if code == '.' && dots != -1 {
			dots++
		} else {
			dots = -1
		}
	}
	return res
}
