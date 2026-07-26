package controller

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/loadtest"
)

// LoadTestRunner drives demo load generation from HTTP handlers.
type LoadTestRunner interface {
	StartCapacityLoad(ctx context.Context, delayMs, vus, durationSeconds int) error
	SetBehavior(ctx context.Context, delayMs, errorRate int, targetPod string) error
	StopLoad(ctx context.Context) error
	Status(ctx context.Context) (loadtest.LoadStatus, error)
	PickTargetPod(ctx context.Context) (string, error)
	StartLoadJob(ctx context.Context, vus, durationSeconds int) error
}

func (c *Controller) handleLoadtestCapacity(w http.ResponseWriter, r *http.Request) {
	if c.loadRunner == nil {
		http.Error(w, "load test runner is not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		DelayMs         int `json:"delayMs"`
		VUs             int `json:"vus"`
		DurationSeconds int `json:"durationSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.DelayMs <= 0 || req.VUs <= 0 || req.DurationSeconds <= 0 {
		http.Error(w, "delayMs, vus, and durationSeconds must be positive", http.StatusBadRequest)
		return
	}
	if err := c.loadRunner.StartCapacityLoad(r.Context(), req.DelayMs, req.VUs, req.DurationSeconds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (c *Controller) handleLoadtestBadPod(w http.ResponseWriter, r *http.Request) {
	if c.loadRunner == nil {
		http.Error(w, "load test runner is not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		TargetPod string `json:"targetPod"`
		ErrorRate int    `json:"errorRate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ErrorRate <= 0 {
		http.Error(w, "errorRate must be positive", http.StatusBadRequest)
		return
	}
	targetPod := req.TargetPod
	if targetPod == "" {
		var err error
		targetPod, err = c.loadRunner.PickTargetPod(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := c.loadRunner.SetBehavior(r.Context(), 0, req.ErrorRate, targetPod); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := c.loadRunner.StartLoadJob(r.Context(), 20, 600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "started", "targetPod": targetPod})
}

func (c *Controller) handleLoadtestStop(w http.ResponseWriter, r *http.Request) {
	if c.loadRunner == nil {
		http.Error(w, "load test runner is not configured", http.StatusServiceUnavailable)
		return
	}
	if err := c.loadRunner.StopLoad(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (c *Controller) handleLoadtestStatus(w http.ResponseWriter, r *http.Request) {
	if c.loadRunner == nil {
		http.Error(w, "load test runner is not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := c.loadRunner.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, status)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
