package claudeacp

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	acpdriver "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/acp"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Claude Code labels more than a refused sign-in "authentication_failed", and its
// ACP adapter turns that label into a bare auth-required error with no text. On
// 2026-11-23 a Vertex model behind the CPA gateway answered some requests with
// 403 "Access requires an eligible paid billing account in a supported region,
// subject to our terms and policies." and others with a normal reply. AO showed
// "Authentication required", raised a sign-in demand nothing could satisfy, and
// the app's automatic retry left the chat alone because sign-in failures need a
// person. Claude Code's own transcript keeps the real error: its last record is
// the synthetic assistant message with isApiErrorMessage and apiErrorStatus.
const (
	// The record is written as the turn fails; a few short waits cover a write
	// that lands just after the adapter's answer.
	claudeAPIErrorReadAttempts = 4
	claudeAPIErrorReadPause    = 250 * time.Millisecond
	// Only a record this recent belongs to the prompt that just failed. A real
	// sign-in refusal that wrote nothing must not borrow an older turn's error.
	claudeAPIErrorMaxAge = 2 * time.Minute
	// The error record is the last line; the tail also has to hold the line
	// before it to know the cut did not land inside the record.
	claudeTranscriptTailBytes = 256 << 10
	claudeAPIErrorMaxText     = 2000
)

var (
	// The CLI's prefix for an authentication_failed error. It is Claude Code's
	// label, not the provider's words, and it is wrong for the errors this file
	// exists for.
	claudeAuthFailedPrefix = regexp.MustCompile(`(?i)^\s*Failed to authenticate\.?\s*`)
	// "Please run /login", "Run /login to sign in again": a real sign-in problem.
	claudeLoginInstruction = regexp.MustCompile(`(?i)\brun /login\b`)
	// "API Error: 403 …" in the CLI's text, for a record without apiErrorStatus.
	claudeAPIErrorStatusText = regexp.MustCompile(`\bAPI Error: (\d{3})\b`)
	// Claude Code's project folder name: every character outside [A-Za-z0-9]
	// becomes "-" (G:\Projects\repo -> G--Projects-repo).
	claudeProjectKeyUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)
)

type claudeTranscriptRecord struct {
	Type              string    `json:"type"`
	IsSidechain       bool      `json:"isSidechain"`
	IsAPIErrorMessage bool      `json:"isApiErrorMessage"`
	APIErrorStatus    int       `json:"apiErrorStatus"`
	Timestamp         time.Time `json:"timestamp"`
	Message           struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// claudeAuthRequiredFailure is the Claude binding's acp.Config.AuthRequiredFailure.
func claudeAuthRequiredFailure(pc acpdriver.AuthRequiredContext) error {
	return claudeAuthRequiredFailureAt(pc, time.Now, time.Sleep)
}

func claudeAuthRequiredFailureAt(
	pc acpdriver.AuthRequiredContext,
	now func() time.Time,
	sleep func(time.Duration),
) error {
	sessionID := strings.TrimSpace(pc.SessionID)
	if !isClaudeSessionID(sessionID) {
		return nil
	}
	for attempt := 1; ; attempt++ {
		if record, ok := latestClaudeAPIError(pc, sessionID, now()); ok {
			return claudeProviderRefusal(record)
		}
		if attempt >= claudeAPIErrorReadAttempts {
			return nil
		}
		sleep(claudeAPIErrorReadPause)
	}
}

// claudeProviderRefusal decides what the recorded API error was. A 401, or text
// that tells the user to sign in, keeps the adapter's auth failure (nil). Any
// other status is the provider refusing the request, reported in its own words
// and without ports.ErrChatAuthRequired.
func claudeProviderRefusal(record claudeTranscriptRecord) error {
	text := claudeRecordText(record.Message.Content)
	if text == "" || claudeLoginInstruction.MatchString(text) {
		return nil
	}
	status := record.APIErrorStatus
	if status == 0 {
		if match := claudeAPIErrorStatusText.FindStringSubmatch(text); match != nil {
			status, _ = strconv.Atoi(match[1])
		}
	}
	if status == 0 || status == 401 {
		return nil
	}
	text = strings.TrimSpace(claudeAuthFailedPrefix.ReplaceAllString(text, ""))
	if text == "" {
		return nil
	}
	if len(text) > claudeAPIErrorMaxText {
		text = text[:claudeAPIErrorMaxText]
	}
	return ports.NewChatProviderFailure(text, "", nil)
}

// latestClaudeAPIError returns the session's newest main-thread assistant record
// when it is a recent API error. Any later real answer means the failure that
// just happened is not that record's.
func latestClaudeAPIError(pc acpdriver.AuthRequiredContext, sessionID string, now time.Time) (claudeTranscriptRecord, bool) {
	path := claudeTranscriptPath(pc, sessionID)
	if path == "" {
		return claudeTranscriptRecord{}, false
	}
	tail, whole, err := readClaudeTranscriptTail(path)
	if err != nil {
		return claudeTranscriptRecord{}, false
	}
	lines := bytes.Split(bytes.TrimRight(tail, "\r\n"), []byte("\n"))
	if !whole && len(lines) > 0 {
		lines = lines[1:] // the cut landed inside this line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var record claudeTranscriptRecord
		if json.Unmarshal(line, &record) != nil || record.Type != "assistant" || record.IsSidechain {
			continue
		}
		if !record.IsAPIErrorMessage {
			return claudeTranscriptRecord{}, false
		}
		age := now.Sub(record.Timestamp)
		if record.Timestamp.IsZero() || age > claudeAPIErrorMaxAge || age < -claudeAPIErrorMaxAge {
			return claudeTranscriptRecord{}, false
		}
		return record, true
	}
	return claudeTranscriptRecord{}, false
}

// claudeTranscriptPath finds <config>/projects/<project key>/<session>.jsonl:
// the workspace's own folder first, then any project folder (Claude shortens
// very long keys, and a session may have been started from another folder).
func claudeTranscriptPath(pc acpdriver.AuthRequiredContext, sessionID string) string {
	configDir := strings.TrimSpace(pc.Env["CLAUDE_CONFIG_DIR"])
	if configDir == "" {
		configDir = strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	}
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configDir = filepath.Join(home, ".claude")
	}
	projects := filepath.Join(configDir, "projects")
	name := sessionID + ".jsonl"
	if workspace := strings.TrimSpace(pc.WorkspacePath); workspace != "" {
		candidate := filepath.Join(projects, claudeProjectKeyUnsafe.ReplaceAllString(workspace, "-"), name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	entries, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(projects, entry.Name(), name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

// readClaudeTranscriptTail reads at most claudeTranscriptTailBytes from the end
// of the transcript; whole reports that it read the file from its start.
func readClaudeTranscriptTail(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	offset := info.Size() - claudeTranscriptTailBytes
	whole := offset <= 0
	if whole {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, claudeTranscriptTailBytes))
	return data, whole, err
}

// claudeRecordText joins a record's text blocks (or takes a string content).
func claudeRecordText(content json.RawMessage) string {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// isClaudeSessionID: Claude Code's session ids are UUIDs, and the id names a
// file, so nothing else is looked up.
func isClaudeSessionID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}
