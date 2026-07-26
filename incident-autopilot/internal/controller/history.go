package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

const historyCap = 50

func isActionableDecision(d model.Decision) bool {
	switch d {
	case model.DecisionScaleUp, model.DecisionScaleDown, model.DecisionQuarantine:
		return true
	default:
		return false
	}
}

func (c *Controller) isRecommendationPendingLocked(rec model.Recommendation) bool {
	if rec.ID == "" || !isActionableDecision(rec.Decision) {
		return false
	}
	action := c.state.LastAction
	if action.RecommendationID != rec.ID {
		return true
	}
	if action.Result == "rejected" {
		return false
	}
	return action.CompletedAt.IsZero()
}

func (c *Controller) markSupersededPendingLocked() {
	for i := range c.state.History {
		if c.state.History[i].Outcome == "pending" {
			c.state.History[i].Outcome = "superseded"
		}
	}
}

func (c *Controller) appendHistoryLocked(rec model.Recommendation, outcome string) {
	c.markSupersededPendingLocked()
	c.state.History = append(c.state.History, model.HistoryEntry{
		Recommendation: rec,
		Outcome:        outcome,
		RecordedAt:     c.now(),
	})
	if len(c.state.History) > historyCap {
		c.state.History = c.state.History[len(c.state.History)-historyCap:]
	}
}

func (c *Controller) updateHistoryOutcomeLocked(id, outcome, operator string) {
	for i := len(c.state.History) - 1; i >= 0; i-- {
		if c.state.History[i].Recommendation.ID != id {
			continue
		}
		c.state.History[i].Outcome = outcome
		if operator != "" {
			c.state.History[i].Action = model.Action{
				RecommendationID: id,
				ApprovedBy:       operator,
				ApprovedAt:       c.now(),
				CompletedAt:      c.now(),
				Result:           outcome,
			}
		}
		return
	}
}

func (c *Controller) historyNewestFirst() []model.HistoryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]model.HistoryEntry, len(c.state.History))
	for i := range c.state.History {
		out[len(c.state.History)-1-i] = c.state.History[i]
	}
	return out
}

// Reject marks a pending recommendation as rejected without publishing a
// scale change.
func (c *Controller) Reject(_ context.Context, id, operator string, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rec := c.state.LastRecommendation
	if rec.ID != id {
		return fmt.Errorf("unknown or superseded recommendation %q", id)
	}
	if !isActionableDecision(rec.Decision) {
		return fmt.Errorf("recommendation %q is not an executable action", id)
	}
	action := c.state.LastAction
	if action.RecommendationID == id {
		if action.Result == "rejected" {
			return nil
		}
		if !action.CompletedAt.IsZero() {
			return fmt.Errorf("recommendation %q already resolved", id)
		}
	}

	c.updateHistoryOutcomeLocked(id, "rejected", operator)
	c.state.LastAction = model.Action{
		RecommendationID: id,
		ApprovedBy:       operator,
		ApprovedAt:       now,
		CompletedAt:      now,
		Result:           "rejected",
	}
	return c.persistStateLocked()
}
