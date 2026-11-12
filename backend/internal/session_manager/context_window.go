package sessionmanager

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ContextWindowRefusal is returned when a session's harness and model take no
// client context window, or the requested one is not a valid size. Reason is the
// validator's own words, fit to show the caller.
type ContextWindowRefusal struct{ Reason string }

func (e *ContextWindowRefusal) Error() string { return "session: " + e.Reason }

// SetAgentContextWindow changes the context window a session's agent starts with.
// It is saved with the session's other launch options and read back by every
// launch and resume, so it applies from the agent's next start; the agent running
// now keeps the window it started with. A caller that wants it at once exits the
// agent and resumes it (ExitAgent, ResumeAgentWithMode): the provider-native
// conversation carries over. Zero returns the agent to its client default.
func (m *Manager) SetAgentContextWindow(ctx context.Context, id domain.SessionID, tokens int64) (domain.SessionRecord, error) {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: %w", id, err)
	}
	if !ok {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: %w", id, ErrNotFound)
	}
	if rec.IsTerminated {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: %w", id, ErrTerminated)
	}
	model, err := m.contextWindowModel(ctx, rec)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: %w", id, err)
	}
	// Checked against the model a resume launches, as the resume checks it again:
	// a window saved here that the resume refuses would leave the agent unable to start.
	if err := ports.ValidateContextWindow(rec.Harness, model, tokens); err != nil {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: %w", id, &ContextWindowRefusal{Reason: err.Error()})
	}
	options, _ := m.gateways.get(m.dataDir, id)
	options.ContextWindow = tokens
	// As Spawn records a context-only session: the window belongs to this model.
	if !options.Enabled() && tokens > 0 && options.Model == "" {
		options.Model = model
	}
	if err := m.gateways.set(m.dataDir, id, options); err != nil {
		return domain.SessionRecord{}, fmt.Errorf("set context window %s: persist: %w", id, err)
	}
	return rec, nil
}

// contextWindowModel is the model a resume of rec launches: the session's own
// durable choice, else its project's default for the harness.
func (m *Manager) contextWindowModel(ctx context.Context, rec domain.SessionRecord) (string, error) {
	if model := strings.TrimSpace(rec.Metadata.Model); model != "" {
		return model, nil
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	return restoredAgentConfig(rec, project.Config).Model, nil
}
