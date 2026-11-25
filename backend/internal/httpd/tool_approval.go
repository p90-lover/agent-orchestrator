package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// toolApprovalPath is where `ao hooks pi tool-call` files a Pi tool call for
// the chat to approve. It is AO-internal (its own CLI to its own daemon), so it
// sits outside the public /api/v1 spec and the LAN listener's routes.
const toolApprovalPath = "/internal/chat/sessions/{sessionId}/tool-approval"

// maxToolApprovalBody bounds one request: a tool call's arguments, which for an
// edit can carry a file's new text.
const maxToolApprovalBody = 4 << 20

// toolApprovalService is the chat service's side of toolApprovalPath.
type toolApprovalService interface {
	RequestToolApproval(context.Context, domain.SessionID, ports.ChatToolApprovalRequest) (ports.ChatToolApprovalResult, error)
}

// ToolApprovalRequest is the body `ao hooks pi tool-call` posts.
type ToolApprovalRequest struct {
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Kind       string          `json:"kind"`
	Title      string          `json:"title"`
	Input      json.RawMessage `json:"input,omitempty"`
}

// ToolApprovalResponse is the answer: allow, or not and why.
type ToolApprovalResponse struct {
	Allow   bool   `json:"allow"`
	Message string `json:"message,omitempty"`
}

// mountToolApproval files a tool call the session's agent asks about outside
// its protocol connection and answers once the chat has (Pi: pi-acp sends no
// permission requests). The request waits as long as the approval is pending;
// the server sets no write timeout. Only a loopback caller with no Origin may
// call it, and the conversation accepts it only with the session's own token.
func mountToolApproval(r chi.Router, conversations any) {
	svc, ok := conversations.(toolApprovalService)
	if !ok {
		return
	}
	r.Post(toolApprovalPath, func(w http.ResponseWriter, req *http.Request) {
		if !localControlRequest(req) {
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden"})
			return
		}
		token, hasBearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !hasBearer || strings.TrimSpace(token) == "" {
			envelope.WriteJSON(w, http.StatusUnauthorized, map[string]any{"status": "unauthorized"})
			return
		}
		var body ToolApprovalRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxToolApprovalBody)).Decode(&body); err != nil {
			envelope.WriteJSON(w, http.StatusBadRequest, map[string]any{"status": "invalid_request"})
			return
		}
		result, err := svc.RequestToolApproval(req.Context(), domain.SessionID(chi.URLParam(req, "sessionId")),
			ports.ChatToolApprovalRequest{
				Token: strings.TrimSpace(token), ToolCallID: body.ToolCallID, ToolName: body.ToolName,
				Kind: body.Kind, Title: body.Title, Input: body.Input,
			})
		switch {
		case errors.Is(err, ports.ErrChatToolApprovalToken):
			envelope.WriteJSON(w, http.StatusForbidden, map[string]any{"status": "forbidden"})
		case err != nil:
			envelope.WriteJSON(w, http.StatusConflict, map[string]any{"status": "failed", "message": err.Error()})
		default:
			envelope.WriteJSON(w, http.StatusOK, ToolApprovalResponse{Allow: result.Allowed, Message: result.Message})
		}
	})
}
