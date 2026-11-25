package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Pi's chat approvals (adapters/chatdriver/piacp/APPROVALS.md). AO's piacp
// gives the pi-acp daemon these, and AO's managed Pi extension runs
// `ao hooks pi tool-call` with that env for each tool call it must ask about.
const (
	piToolCallHookEvent      = "tool-call"
	envPiApprovalSessionID   = "AO_PI_APPROVAL_SESSION_ID"
	envPiApprovalToken       = "AO_PI_APPROVAL_TOKEN"
	piToolApprovalWait       = 25 * time.Hour // past the chat's own 24 h approval wait
	maxPiToolApprovalTitle   = 200
	piToolApprovalDeniedText = "Denied in the chat"
)

// piToolCallPayload is what the managed extension writes to stdin: Pi's
// tool_call event.
type piToolCallPayload struct {
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Input      json.RawMessage `json:"input"`
}

// piToolCallAnswer is the one JSON line the extension reads back.
type piToolCallAnswer struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

func isPiToolCallHook(agent, event string) bool {
	return domain.AgentHarness(agent) == domain.HarnessPi && event == piToolCallHookEvent
}

// runPiToolCallHook asks the chat about one Pi tool call and prints the
// answer. It always prints one, and anything but an allow from the chat is a
// deny: the extension blocks the tool on it, so a failure never runs a tool
// unseen.
func (c *commandContext) runPiToolCallHook(ctx context.Context) error {
	answer := c.askChatAboutPiToolCall(ctx)
	return json.NewEncoder(c.deps.Out).Encode(answer)
}

func (c *commandContext) askChatAboutPiToolCall(ctx context.Context) piToolCallAnswer {
	sessionID := strings.TrimSpace(os.Getenv(envPiApprovalSessionID))
	token := strings.TrimSpace(os.Getenv(envPiApprovalToken))
	if !sessionIDPattern.MatchString(sessionID) || token == "" {
		return piToolCallAnswer{Reason: "AO could not ask the chat: this Pi session has no chat approval settings"}
	}
	raw, err := io.ReadAll(io.LimitReader(c.deps.In, 4<<20))
	if err != nil {
		return piToolCallAnswer{Reason: fmt.Sprintf("AO could not ask the chat: read the tool call: %v", err)}
	}
	var call piToolCallPayload
	if err := json.Unmarshal(raw, &call); err != nil || strings.TrimSpace(call.ToolName) == "" {
		return piToolCallAnswer{Reason: "AO could not ask the chat: the tool call was not readable"}
	}
	kind, title := piToolApprovalSubject(call)
	var response struct {
		Allow   bool   `json:"allow"`
		Message string `json:"message"`
	}
	err = c.doJSONPathWithHeadersAndTimeout(ctx, http.MethodPost,
		"/internal/chat/sessions/"+url.PathEscape(sessionID)+"/tool-approval",
		map[string]any{
			"toolCallId": call.ToolCallID, "toolName": call.ToolName, "kind": kind, "title": title,
			"input": call.Input,
		},
		&response, map[string]string{"Authorization": "Bearer " + token}, piToolApprovalWait)
	if err != nil {
		return piToolCallAnswer{Reason: fmt.Sprintf("AO could not ask the chat: %v", err)}
	}
	if response.Allow {
		return piToolCallAnswer{Allow: true}
	}
	reason := strings.TrimSpace(response.Message)
	if reason == "" {
		reason = piToolApprovalDeniedText
	}
	return piToolCallAnswer{Reason: reason}
}

// piToolApprovalSubject files a Pi tool under an ACP tool kind (which decides
// what accept edits answers by itself) and names it for the approval card.
func piToolApprovalSubject(call piToolCallPayload) (kind, title string) {
	var input map[string]any
	_ = json.Unmarshal(call.Input, &input)
	text := func(key string) string {
		value, _ := input[key].(string)
		return strings.TrimSpace(value)
	}
	switch call.ToolName {
	case "bash":
		kind, title = "execute", text("command")
	case "edit", "write":
		kind, title = "edit", text("path")
	default:
		kind = "other"
	}
	if title == "" {
		title = call.ToolName
	}
	if len(title) > maxPiToolApprovalTitle {
		title = strings.ToValidUTF8(title[:maxPiToolApprovalTitle], "") + "…"
	}
	return kind, title
}
