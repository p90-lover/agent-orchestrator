package codexappserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/codexappserver/codexproto"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// handleGoalNotification observes semantic /goal edits. The provider also emits
// notifications for usage accounting; those must never trigger another model turn.
// Only steer a turn that the provider has acknowledged as running. Idle threads
// already receive the saved goal when their next turn starts.
func (c *conversation) handleGoalNotification(n notification) {
	if n.Method != codexproto.MethodThreadGoalUpdated && n.Method != codexproto.MethodThreadGoalCleared {
		return
	}
	var update codexproto.ThreadGoalUpdatedNotification
	if n.Method == codexproto.MethodThreadGoalUpdated {
		if json.Unmarshal(n.Params, &update) != nil || update.ThreadID != c.threadID {
			return
		}
	} else {
		var clear codexproto.ThreadGoalClearedNotification
		if json.Unmarshal(n.Params, &clear) != nil || clear.ThreadID != c.threadID {
			return
		}
		update.ThreadID = clear.ThreadID
		update.Goal = codexproto.ThreadGoal{Status: codexproto.ThreadGoalStatusComplete}
	}

	objective := strings.TrimSpace(update.Goal.Objective)
	status := update.Goal.Status
	c.mu.Lock()
	if c.goalSeen && c.goalObjective == objective && c.goalStatus == status {
		c.mu.Unlock()
		return
	}
	c.goalSeen = true
	c.goalObjective, c.goalStatus = objective, status
	c.goalRevision++
	revision := c.goalRevision
	turnID := c.activeTurn
	started := c.turnStarted
	match := update.TurnID == nil || *update.TurnID == turnID
	if !started && turnID != "" && match {
		c.goalPendingTurn, c.goalPendingRevision = turnID, revision
	}
	c.mu.Unlock()
	if !started || turnID == "" || !match {
		return
	}
	go c.steerGoalRevision(turnID, revision)
}

// Serialize goal steers. If goals A and B are edited quickly, the steer for B
// must never be followed by a delayed, stale steer for A. A rejected steer does
// not restart the task or create a duplicate turn.
func (c *conversation) steerGoalRevision(turnID string, revision uint64) {
	c.goalSteerMu.Lock()
	defer c.goalSteerMu.Unlock()
	c.mu.Lock()
	if c.closed || !c.turnStarted || c.activeTurn != turnID || revision != c.goalRevision {
		c.mu.Unlock()
		return
	}
	objective, status := c.goalObjective, c.goalStatus
	c.mu.Unlock()
	message := "The user edited /goal while this turn was running. Re-evaluate your plan now against the revised goal. " +
		"Stop or redirect work that conflicts with it; retain work that is still relevant. Do not continue the old objective without reconsidering it."
	if objective == "" {
		message += " The goal was cleared; stop pursuing the previous objective."
	} else {
		message += fmt.Sprintf("\nLatest goal (status: %s): %s", status, objective)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := c.Steer(ctx, turnID, ports.ChatUserMessage{Text: message}); err != nil {
		c.log.Warn("unable to deliver edited /goal to active turn", "error", err)
	}
}
