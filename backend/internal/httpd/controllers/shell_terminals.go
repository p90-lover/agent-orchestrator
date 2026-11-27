package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/requestscope"
	shelltermsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

// ShellTerminalService is the controller-facing standalone shell terminal
// contract.
type ShellTerminalService interface {
	OpenShellTerminal(ctx context.Context, in shelltermsvc.OpenShellTerminalInput) (shelltermsvc.ShellTerminal, error)
	ListShellTerminalsForCurrentAppRun(ctx context.Context) ([]shelltermsvc.ShellTerminal, error)
	RenameShellTerminal(ctx context.Context, handleID, title string) (shelltermsvc.ShellTerminal, error)
	CloseShellTerminal(ctx context.Context, handleID string) error

	// The in-app SSH terminal (see internal/service/shellterm/ssh.go).
	OpenSSHTerminal(ctx context.Context, in shelltermsvc.OpenSSHTerminalInput) (shelltermsvc.ShellTerminal, error)
	SSHTerminalEnabled() bool
	SSHTerminalTokenMatches(presented string) bool
	// IsProtectedTerminal is the /mux gate's authority; it is part of this
	// contract so whatever is wired here always protects SSH terminals there.
	IsProtectedTerminal(id string) bool
}

// maxSSHTerminalRequestBytes bounds the SSH terminal request body.
const maxSSHTerminalRequestBytes = 16 << 10

// ShellTerminalsController owns the /shell-terminals routes: standalone shells
// the user opens by hand, independent of any agent session.
type ShellTerminalsController struct {
	Svc ShellTerminalService
}

// Register mounts the bounded shell terminal REST routes.
func (c *ShellTerminalsController) Register(r chi.Router) {
	r.Get("/shell-terminals", c.list)
	r.Post("/shell-terminals", c.open)
	r.Post("/shell-terminals/ssh", c.openSSH)
	r.Patch("/shell-terminals/{handleId}", c.rename)
	r.Delete("/shell-terminals/{handleId}", c.close)
}

// sshAccessContext marks the request context for the SSH terminal gate when it
// arrived on the loopback listener with this run's capability. Any other
// request keeps its context, and the service then treats an SSH terminal as
// unknown.
func (c *ShellTerminalsController) sshAccessContext(r *http.Request) context.Context {
	ctx := r.Context()
	if requestscope.IsLAN(ctx) || !c.Svc.SSHTerminalTokenMatches(r.Header.Get(shelltermsvc.SSHTerminalTokenHeader)) {
		return ctx
	}
	return shelltermsvc.WithSSHTerminalAccess(ctx)
}

// openSSH opens the in-app SSH terminal. The checks run in the order the route
// documents: a LAN caller, the route off, the capability, then the body (read
// here strictly, then judged field by field by the service with the pinned
// keys file and the ssh program).
func (c *ShellTerminalsController) openSSH(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/shell-terminals/ssh")
		return
	}
	if requestscope.IsLAN(r.Context()) {
		envelope.WriteError(w, r, shelltermsvc.NewSSHTerminalLocalOnlyError())
		return
	}
	if !c.Svc.SSHTerminalEnabled() {
		envelope.WriteError(w, r, shelltermsvc.NewSSHTerminalDisabledError())
		return
	}
	if !c.Svc.SSHTerminalTokenMatches(r.Header.Get(shelltermsvc.SSHTerminalTokenHeader)) {
		envelope.WriteError(w, r, shelltermsvc.NewSSHTerminalForbiddenError())
		return
	}
	in, err := decodeSSHTerminalRequest(w, r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	terminal, err := c.Svc.OpenSSHTerminal(shelltermsvc.WithSSHTerminalAccess(r.Context()), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

// sshTerminalRequestFields and sshClientVersionFields are the exact key names
// the body may carry, each once. encoding/json matches keys without regard to
// case and keeps the last of two equal keys, so the names are checked as
// written, on the token stream, before the body is decoded.
var (
	sshTerminalRequestFields = map[string]bool{"host": true, "port": true, "user": true, "identityFile": true, "title": true, "client": true}
	sshClientVersionFields   = map[string]bool{"major": true, "minor": true}
)

// sshTerminalFieldLabels name each field the way the app's messages do.
var sshTerminalFieldLabels = map[string]string{
	"host": "Host", "port": "Port", "user": "User", "identityFile": "Key file", "title": "Title", "client": "Client",
}

// sshTerminalNullRefusals are the fields that refuse null, with the message
// each answers. port and identityFile take null as "not given" (22, no key
// file); the spec says the same (openapi.yaml, OpenSSHTerminalRequest).
var sshTerminalNullRefusals = map[string]string{
	"host":   "Host: enter a host name or an IP address.",
	"user":   "User: enter the user name you sign in with on the server.",
	"title":  "Title: enter a name for this terminal.",
	"client": sshClientProblem,
}

const (
	sshNotJSON        = "The request is not valid JSON."
	sshNotOneObject   = "Send the host, port, user and title as one JSON object."
	sshExtraArguments = "SSH terminals take no extra options or arguments."
	sshClientProblem  = "Client: send the OpenSSH version as two whole numbers from 0 to 999 (major and minor)."
)

// sshTerminalContentTypeOK accepts application/json with, at most, a utf-8
// charset (both case-insensitive).
func sshTerminalContentTypeOK(header string) bool {
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil || mediaType != "application/json" {
		return false
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

// decodeSSHTerminalRequest reads the body strictly: JSON by its content type,
// at most 16 KiB of UTF-8, exactly one object, only the documented keys and
// each once, null only where it means "not given", each value of its JSON
// type. Each refusal is a 400 SSH_TERMINAL_INVALID that names the field.
func decodeSSHTerminalRequest(w http.ResponseWriter, r *http.Request) (shelltermsvc.OpenSSHTerminalInput, error) {
	invalid := shelltermsvc.NewSSHTerminalInvalidError
	if !sshTerminalContentTypeOK(r.Header.Get("Content-Type")) {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid("Send the request as JSON (Content-Type: application/json).")
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSSHTerminalRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return shelltermsvc.OpenSSHTerminalInput{}, invalid("The request is too large.")
		}
		return shelltermsvc.OpenSSHTerminalInput{}, invalid("The request could not be read.")
	}
	// A string the app sends is always valid UTF-16 and UTF-8; the decoder
	// would turn anything else into U+FFFD, which the app's rules never see.
	if !utf8.Valid(body) || hasLoneSurrogateEscape(body) {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid("The request is not valid text.")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid("Send the host, port, user and title of the SSH terminal.")
	}
	if problem := sshTerminalKeyProblem(body); problem != "" {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid(problem)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var req OpenSSHTerminalRequest
	if err := decoder.Decode(&req); err != nil {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid(sshTerminalDecodeProblem(err))
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return shelltermsvc.OpenSSHTerminalInput{}, invalid("Send one JSON object.")
	}
	in := shelltermsvc.OpenSSHTerminalInput{
		Host:  req.Host,
		Port:  req.Port,
		User:  req.User,
		Title: req.Title,
	}
	if req.IdentityFile != nil {
		in.IdentityFile = *req.IdentityFile
	}
	if req.Client != nil {
		// Both are present and numbers: the key check refused anything else.
		major, minor := req.Client.Major, req.Client.Minor
		in.Client = &shelltermsvc.SSHClientVersionInput{Major: &major, Minor: &minor}
	}
	return in, nil
}

// sshTerminalKeyProblem walks the first JSON value as a token stream: an
// object (not null, not anything else), only the documented keys and each
// once, null only for port and identityFile, and inside client exactly major
// and minor, each once and not null. Value types are judged by the decode
// that follows.
func sshTerminalKeyProblem(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return sshNotJSON
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return sshNotOneObject
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return sshNotJSON
		}
		name, _ := tok.(string)
		if !sshTerminalRequestFields[name] {
			return sshExtraArguments
		}
		if seen[name] {
			return sshTerminalFieldLabels[name] + ": send this field once."
		}
		seen[name] = true
		value, err := dec.Token()
		if err != nil {
			return sshNotJSON
		}
		if value == nil {
			if message, refused := sshTerminalNullRefusals[name]; refused {
				return message
			}
			continue
		}
		delim, container := value.(json.Delim)
		if !container {
			continue
		}
		if name == "client" && delim == '{' {
			if problem := sshClientKeyProblem(dec); problem != "" {
				return problem
			}
			continue
		}
		if err := skipJSONContainer(dec); err != nil {
			return sshNotJSON
		}
	}
	if _, err := dec.Token(); err != nil {
		return sshNotJSON
	}
	return ""
}

// sshClientKeyProblem walks client's object after its opening brace.
func sshClientKeyProblem(dec *json.Decoder) string {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return sshNotJSON
		}
		name, _ := tok.(string)
		if !sshClientVersionFields[name] {
			return sshClientProblem
		}
		if seen[name] {
			return "Client: send major and minor once each."
		}
		seen[name] = true
		value, err := dec.Token()
		if err != nil {
			return sshNotJSON
		}
		if _, container := value.(json.Delim); container || value == nil {
			return sshClientProblem
		}
	}
	if _, err := dec.Token(); err != nil {
		return sshNotJSON
	}
	if !seen["major"] || !seen["minor"] {
		return sshClientProblem
	}
	return ""
}

// skipJSONContainer consumes the rest of an object or array whose opening
// delimiter was just read.
func skipJSONContainer(dec *json.Decoder) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// sshTerminalDecodeProblem words a decode failure for the field it names, with
// the app's sentence for a value of the wrong type.
func sshTerminalDecodeProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		switch typeErr.Field {
		case "host":
			return "Host: enter a host name or an IP address."
		case "port":
			if typeErr.Value == "string" {
				return "Port: send the port as a number from 1 to 65535. Leave it out for 22."
			}
			return "Port: use digits only, from 1 to 65535. Leave it empty for 22."
		case "user":
			return "User: enter the user name you sign in with on the server."
		case "identityFile":
			return "Key file: choose the private key file by its full path."
		case "title":
			return "Title: enter a name for this terminal."
		case "client", "client.major", "client.minor":
			return sshClientProblem
		}
		return sshNotOneObject
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return sshExtraArguments
	}
	return sshNotJSON
}

// hasLoneSurrogateEscape reports a \uD800-\uDFFF escape that is not half of a
// pair. JSON allows it, the decoder replaces it with U+FFFD, and the app's
// rules would have judged the lone surrogate itself.
func hasLoneSurrogateEscape(body []byte) bool {
	hexValue := func(at int) (int, bool) {
		if at+4 > len(body) {
			return 0, false
		}
		value, err := strconv.ParseUint(string(body[at:at+4]), 16, 16)
		return int(value), err == nil
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		if i+1 >= len(body) {
			return false
		}
		if body[i+1] != 'u' {
			i++ // an escaped character, a backslash among them
			continue
		}
		unit, ok := hexValue(i + 2)
		if !ok {
			return false // malformed: the decoder refuses it
		}
		i += 5
		switch {
		case unit >= 0xdc00 && unit <= 0xdfff:
			return true
		case unit >= 0xd800 && unit <= 0xdbff:
			if i+2 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return true
			}
			low, ok := hexValue(i + 3)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return true
			}
			i += 6
		}
	}
	return false
}

func (c *ShellTerminalsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/shell-terminals")
		return
	}
	terminals, err := c.Svc.ListShellTerminalsForCurrentAppRun(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListShellTerminalsResponse{
		ShellTerminals: shellTerminalResponses(terminals),
	})
}

func (c *ShellTerminalsController) open(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/shell-terminals")
		return
	}
	// An empty body is a valid request: it means "open a shell with no project
	// context", which the service resolves to the daemon data dir.
	var req OpenShellTerminalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	terminal, err := c.Svc.OpenShellTerminal(r.Context(), shelltermsvc.OpenShellTerminalInput{
		ProjectID: domain.ProjectID(req.ProjectID),
		SessionID: domain.SessionID(req.SessionID),
		Shell:     req.Shell,
		Env:       req.Env,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

func (c *ShellTerminalsController) rename(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/shell-terminals/{handleId}")
		return
	}
	var req UpdateShellTerminalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	handleID, err := url.PathUnescape(chi.URLParam(r, "handleId"))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "SHELL_TERMINAL_ID_INVALID", "Invalid shell terminal id", nil)
		return
	}
	terminal, err := c.Svc.RenameShellTerminal(c.sshAccessContext(r), handleID, req.Title)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ShellTerminalEnvelope{
		ShellTerminal: shellTerminalResponse(terminal),
	})
}

func (c *ShellTerminalsController) close(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/shell-terminals/{handleId}")
		return
	}
	handleID, err := url.PathUnescape(chi.URLParam(r, "handleId"))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "SHELL_TERMINAL_ID_INVALID", "Invalid shell terminal id", nil)
		return
	}
	if err := c.Svc.CloseShellTerminal(c.sshAccessContext(r), handleID); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func shellTerminalResponses(in []shelltermsvc.ShellTerminal) []ShellTerminalResponse {
	out := make([]ShellTerminalResponse, 0, len(in))
	for _, t := range in {
		out = append(out, shellTerminalResponse(t))
	}
	return out
}

func shellTerminalResponse(t shelltermsvc.ShellTerminal) ShellTerminalResponse {
	return ShellTerminalResponse{
		HandleID:   t.HandleID,
		ProjectID:  string(t.ProjectID),
		SessionID:  string(t.SessionID),
		WorkingDir: t.WorkingDir,
		Title:      t.Title,
		CreatedAt:  t.CreatedAt,
	}
}
