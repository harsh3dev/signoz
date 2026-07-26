package controller

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

type statusSnapshot struct {
	SLI         float64   `json:"sli"`
	P95MS       float64   `json:"p95Ms"`
	ErrorRate   float64   `json:"errorRate"`
	RequestRate float64   `json:"requestRate"`
	ObservedAt  time.Time `json:"observedAt"`
}

type pendingRecommendation struct {
	ID                  string          `json:"id"`
	Decision            model.Decision  `json:"decision"`
	CurrentReplicas     int32           `json:"currentReplicas"`
	RecommendedReplicas int32           `json:"recommendedReplicas"`
	TargetPod           string          `json:"targetPod,omitempty"`
	Reason              string          `json:"reason"`
	ExpiresAt           time.Time       `json:"expiresAt"`
	Token               string          `json:"token"`
}

type statusResponse struct {
	CurrentReplicas           int32                `json:"currentReplicas"`
	AvailableReplicas         int32                `json:"availableReplicas"`
	RecommendedReplicas       int32                `json:"recommendedReplicas"`
	LastDecision              model.Decision       `json:"lastDecision"`
	LastSnapshot              statusSnapshot       `json:"lastSnapshot"`
	TelemetryFreshnessSeconds float64              `json:"telemetryFreshnessSeconds"`
	FreshnessLimitSeconds     float64              `json:"freshnessLimitSeconds"`
	Mode                      string               `json:"mode"`
	Drift                     bool                 `json:"drift"`
	Pods                      []string             `json:"pods"`
	TelemetryAvailable        bool                 `json:"telemetryAvailable"`
	PendingRecommendation     *pendingRecommendation `json:"pendingRecommendation,omitempty"`
}

func (c *Controller) handleStatus(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	snapshot := c.state.LastSnapshot
	rec := c.state.LastRecommendation
	published := c.state.PublishedReplicas
	pending := c.isRecommendationPendingLocked(rec)
	c.mu.Unlock()

	currentReplicas := snapshot.CurrentReplicas
	availableReplicas := snapshot.Available
	if c.replicas != nil {
		if live, err := c.replicas.Replicas(r.Context()); err == nil {
			currentReplicas = live.Current
			availableReplicas = live.Available
		}
	}

	freshness := 0.0
	if !snapshot.ObservedAt.IsZero() {
		freshness = time.Since(snapshot.ObservedAt).Seconds()
	}

	pods := make([]string, 0, len(snapshot.Pods))
	for _, p := range snapshot.Pods {
		pods = append(pods, p.Name)
	}

	resp := statusResponse{
		CurrentReplicas:     currentReplicas,
		AvailableReplicas:   availableReplicas,
		RecommendedReplicas: published,
		LastDecision:        rec.Decision,
		LastSnapshot: statusSnapshot{
			SLI:         snapshot.SLI,
			P95MS:       snapshot.P95MS,
			ErrorRate:   snapshot.ErrorRate,
			RequestRate: snapshot.RequestRate,
			ObservedAt:  snapshot.ObservedAt,
		},
		TelemetryFreshnessSeconds: freshness,
		FreshnessLimitSeconds:     c.cfg.Signals.FreshnessLimit.Seconds(),
		Mode:                      c.cfg.Controller.Mode,
		Drift:                     currentReplicas != published,
		Pods:                      pods,
		TelemetryAvailable:        !snapshot.ObservedAt.IsZero(),
	}

	if pending {
		resp.PendingRecommendation = &pendingRecommendation{
			ID:                  rec.ID,
			Decision:            rec.Decision,
			CurrentReplicas:     rec.CurrentReplicas,
			RecommendedReplicas: rec.RecommendedReplicas,
			TargetPod:           rec.TargetPod,
			Reason:              rec.Reason,
			ExpiresAt:           rec.ExpiresAt,
			Token:               SignApproval(c.approvalSecret, rec.ID, rec.ExpiresAt),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *Controller) handleListActions(w http.ResponseWriter, _ *http.Request) {
	entries := c.historyNewestFirst()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (c *Controller) handleReject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	operator := r.Header.Get("X-Autopilot-Operator")
	if operator == "" {
		http.Error(w, "missing X-Autopilot-Operator", http.StatusBadRequest)
		return
	}
	if err := c.Reject(r.Context(), id, operator, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "rejected", "id": id})
}
