package acp

import (
	"context"
	"errors"
	"fmt"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// acpResourceNotFound is JSON-RPC -32002. ACP agents answer it for a session
// the connection does not hold, with data.sessionId naming that session.
const acpResourceNotFound = -32002

// sessionReopen is the session/resume request that reopens this conversation's
// own provider session on the live connection after the agent dropped it. It
// repeats the parameters AO opened the session with, so the agent rebuilds it
// with the same workspace, directories, MCP servers and standing instructions.
type sessionReopen struct {
	meta                  map[string]any
	cwd                   string
	additionalDirectories []string
	mcpServers            []acpsdk.McpServer
}

// isACPSessionGone reports whether err is the agent saying it no longer holds
// sessionID: -32002 "Resource not found" whose data names that session.
// claude-agent-acp evicts a session whose Claude process died and then answers
// every request for it this way, while the adapter process, and so AO's
// connection to it, stays up. A -32002 for another session or a resource is
// not this conversation's loss.
func isACPSessionGone(err error, sessionID string) bool {
	var requestErr *acpsdk.RequestError
	if sessionID == "" || !errors.As(err, &requestErr) || requestErr.Code != acpResourceNotFound {
		return false
	}
	data, _ := requestErr.Data.(map[string]any)
	gone, _ := data["sessionId"].(string)
	return gone == sessionID
}

// enableSessionReopen lets the conversation reopen its session in place when
// the agent advertises session/resume. session/load would also reopen it, but
// it replays the whole transcript as live updates, which a running
// conversation would take for new output.
func (c *conversation) enableSessionReopen(init acpsdk.InitializeResponse, reopen sessionReopen) {
	if init.AgentCapabilities.SessionCapabilities.Resume == nil {
		return
	}
	c.mu.Lock()
	c.reopen = &reopen
	c.mu.Unlock()
}

// noteSessionGone records that err is the agent saying it dropped this
// conversation's session, and reports whether it is. Until the session is
// reopened, no turn can run on it, so the controller is not ready.
func (c *conversation) noteSessionGone(err error) bool {
	if err == nil {
		return false
	}
	c.mu.Lock()
	sessionID := c.sessionID
	gone := isACPSessionGone(err, sessionID)
	first := gone && !c.sessionGone
	if gone {
		c.sessionGone = true
	}
	c.mu.Unlock()
	if first {
		c.log.Warn("chat: ACP agent dropped the session; it is reopened before the next turn",
			"providerSessionID", sessionID, "error", err)
	}
	return gone
}

// idleControllerState is the state a conversation with no turn in flight is in:
// ready, or recovering while its provider session is gone.
func (c *conversation) idleControllerState() ports.ChatControllerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionGone {
		return ports.ChatControllerRecovering
	}
	return ports.ChatControllerReady
}

// applySettingsReopening applies a turn's settings to the provider session,
// reopening it first when the agent dropped it. The follow-up then continues
// the same conversation, whose context the agent restores from its own
// transcript, instead of failing on a session id the agent no longer holds.
func (c *conversation) applySettingsReopening(ctx context.Context, settings ports.ChatTurnSettings) error {
	if err := c.reopenSession(ctx); err != nil {
		return err
	}
	err := c.applyTurnSettings(ctx, settings)
	if !c.noteSessionGone(err) {
		return err
	}
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerRecovering})
	if err := c.reopenSession(ctx); err != nil {
		return err
	}
	err = c.applyTurnSettings(ctx, settings)
	if c.noteSessionGone(err) {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerRecovering})
		return fmt.Errorf("%w: the ACP agent dropped the session again right after reopening it: %w",
			ports.ErrChatResumeFailed, err)
	}
	return err
}

// reopenSession reopens a session the agent dropped with session/resume on the
// same connection. It does nothing while the agent still holds the session.
// A failure is ErrChatResumeFailed (ErrChatAuthRequired for a refused sign-in):
// the conversation could not be continued right now, which a client offers a
// choice about. It is not an internal error, and the conversation stays
// recovering, so the next turn tries again.
func (c *conversation) reopenSession(ctx context.Context) error {
	c.reopenMu.Lock()
	defer c.reopenMu.Unlock()
	c.mu.Lock()
	sessionID, gone, reopen, closed := c.sessionID, c.sessionGone, c.reopen, c.closed
	c.mu.Unlock()
	if !gone {
		return nil
	}
	if closed {
		return errConversationClosed
	}
	if reopen == nil {
		return fmt.Errorf("%w: the ACP agent dropped session %q and does not support session/resume",
			ports.ErrChatResumeFailed, sessionID)
	}
	resumeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	started := time.Now()
	resp, err := c.conn.ResumeSession(resumeCtx, acpsdk.ResumeSessionRequest{
		Meta:                  reopen.meta,
		SessionId:             acpsdk.SessionId(sessionID),
		Cwd:                   reopen.cwd,
		AdditionalDirectories: reopen.additionalDirectories,
		McpServers:            reopen.mcpServers,
	})
	if err != nil {
		c.log.Warn("chat: could not reopen the dropped ACP session",
			"providerSessionID", sessionID, "duration", time.Since(started), "error", err)
		if isACPAuthRequired(err) {
			return normalizeACPError("ACP session/resume", err)
		}
		return fmt.Errorf("%w: reopen dropped ACP session %q: %w", ports.ErrChatResumeFailed, sessionID, err)
	}
	var models *legacySessionModelState
	if c.legacyWire != nil {
		models = c.legacyWire.modelState()
	}
	c.mu.Lock()
	// A new provider process starts from its own defaults; the caller applies
	// the turn's settings again on top of this catalog.
	if len(resp.ConfigOptions) > 0 || models != nil || resp.Modes != nil {
		c.configOptions = normalizeSessionOptions(resp.ConfigOptions, models, resp.Modes)
	}
	if len(c.configOptions) > 0 {
		c.capabilities[ports.ChatCapabilityConfigOptions] = true
	}
	c.sessionGone = false
	c.mu.Unlock()
	c.log.Info("chat: reopened the dropped ACP session",
		"providerSessionID", sessionID, "duration", time.Since(started))
	c.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
	return nil
}
