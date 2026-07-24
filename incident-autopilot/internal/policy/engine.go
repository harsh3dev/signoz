// Package policy classifies a service snapshot into an explainable, bounded
// scaling or remediation recommendation using deterministic rules only.
package policy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

// PolicyVersion identifies the decision logic that produced a recommendation,
// so incident reports remain meaningful after future policy changes.
const PolicyVersion = "v1"

type Engine struct {
	cfg          config.Config
	lastActionAt time.Time
	outlierRuns  map[string]int
	healthyRuns  int
}

func New(cfg config.Config) *Engine {
	return &Engine{
		cfg:         cfg,
		outlierRuns: make(map[string]int),
	}
}

// RecordAction marks that an approved action was executed at `at`, starting
// the cooldown window before another scaling action may be recommended.
func (e *Engine) RecordAction(at time.Time) {
	e.lastActionAt = at
}

// Evaluate classifies a snapshot into a bounded, explainable recommendation.
// It never recommends beyond the configured min/max replicas or per-action
// step limits, and it holds rather than acts on unavailable or rolling-out
// snapshots.
func (e *Engine) Evaluate(s model.Snapshot, now time.Time) model.Recommendation {
	rec := model.Recommendation{
		ID:                  fmt.Sprintf("rec-%d", now.UnixNano()),
		CreatedAt:           now,
		ExpiresAt:           now.Add(5 * time.Minute),
		CurrentReplicas:     s.CurrentReplicas,
		RecommendedReplicas: s.CurrentReplicas,
		PolicyVersion:       PolicyVersion,
	}

	// 1. Reject unavailable snapshots before classifying anything else.
	if s.CurrentReplicas <= 0 || s.Available <= 0 {
		rec.Decision = model.DecisionIndeterminate
		rec.Reason = "No available replicas reported; cannot evaluate scaling safely."
		return rec
	}

	// 2. Hold during rollout or cooldown.
	if s.Available < s.CurrentReplicas {
		e.healthyRuns = 0
		rec.Decision = model.DecisionHold
		rec.Reason = fmt.Sprintf("Rollout in progress: %d of %d replicas available.", s.Available, s.CurrentReplicas)
		return rec
	}
	if !e.lastActionAt.IsZero() && now.Sub(e.lastActionAt) < e.cfg.Policy.Cooldown {
		rec.Decision = model.DecisionHold
		rec.Reason = fmt.Sprintf("Cooldown active since %s; next action eligible after %s.",
			e.lastActionAt.Format(time.RFC3339), e.cfg.Policy.Cooldown)
		return rec
	}

	// 3. Repeated pod outlier with sufficient traffic takes priority over
	// service-wide capacity or error classification.
	outlierPod, isOutlier := detectPodOutlier(s.Pods,
		e.cfg.Policy.PodOutlier.MinimumRequests,
		e.cfg.Policy.PodOutlier.ErrorRateMultiplier,
		e.cfg.Policy.PodOutlier.LatencyMultiplier)
	if isOutlier {
		for name := range e.outlierRuns {
			if name != outlierPod.Name {
				delete(e.outlierRuns, name)
			}
		}
		e.outlierRuns[outlierPod.Name]++
		if e.outlierRuns[outlierPod.Name] >= e.cfg.Policy.PodOutlier.ConsecutiveEvaluations {
			rec.Decision = model.DecisionQuarantine
			rec.TargetPod = outlierPod.Name
			rec.Confidence = 0.9
			rec.Reason = fmt.Sprintf(
				"Pod outlier: %s error rate %.2f%% and P95 latency %.0fms diverge sharply from peers across %d consecutive evaluations.",
				outlierPod.Name, outlierPod.ErrorRate*100, outlierPod.P95MS, e.outlierRuns[outlierPod.Name],
			)
			return rec
		}
	} else {
		e.outlierRuns = make(map[string]int)
	}

	// 4. Low-traffic widespread errors indicate a functional failure that
	// scaling cannot fix.
	p := e.cfg.Policy
	lowTraffic := s.RequestRate < float64(p.PodOutlier.MinimumRequests)
	errorsUnhealthy := s.ErrorRate > p.ErrorRateTarget
	if lowTraffic && errorsUnhealthy {
		e.healthyRuns = 0
		rec.Decision = model.DecisionInvestigate
		rec.Reason = fmt.Sprintf(
			"Low-traffic errors: request rate %.1f/s is too low for capacity to explain a %.2f%% error rate above the %.2f%% target; likely a functional failure.",
			s.RequestRate, s.ErrorRate*100, p.ErrorRateTarget*100,
		)
		return rec
	}

	// 5-7. Capacity-based scale-up, clamped to configured bounds.
	baseReplicas := int32(math.Ceil(s.RequestRate / p.TargetRequestsPerReplica))
	if baseReplicas < 1 {
		baseReplicas = 1
	}
	desired := baseReplicas
	latencyUnhealthy := p.LatencyTargetMS > 0 && s.P95MS > p.LatencyTargetMS
	if latencyUnhealthy {
		desired++
	}
	sliUnhealthy := e.cfg.Signals.SLIObjective > 0 && s.SLI < e.cfg.Signals.SLIObjective

	if desired > s.CurrentReplicas {
		clamped := clampReplicas(desired, p.MinReplicas, p.MaxReplicas)
		if maxUp := s.CurrentReplicas + p.MaxScaleUpStep; clamped > maxUp {
			clamped = maxUp
		}
		if clamped > s.CurrentReplicas {
			evidence := []string{fmt.Sprintf(
				"request rate %.1f/s requires %d replicas at %.1f/s per replica",
				s.RequestRate, baseReplicas, p.TargetRequestsPerReplica,
			)}
			if latencyUnhealthy {
				evidence = append(evidence, fmt.Sprintf("P95 latency %.0fms exceeds %.0fms", s.P95MS, p.LatencyTargetMS))
			}
			if sliUnhealthy {
				evidence = append(evidence, fmt.Sprintf("SLI %.2f%% is below %.2f%%", s.SLI*100, e.cfg.Signals.SLIObjective*100))
			}
			e.healthyRuns = 0
			rec.Decision = model.DecisionScaleUp
			rec.RecommendedReplicas = clamped
			rec.Confidence = 0.85
			rec.Reason = "Capacity pressure: " + strings.Join(evidence, "; ") + "."
			return rec
		}
	}

	// 8. Conservative scale-down: only after three consecutive healthy
	// evaluations, and never by more than MaxScaleDownStep at a time.
	allHealthy := !latencyUnhealthy && !sliUnhealthy && s.ErrorRate <= p.ErrorRateTarget
	if allHealthy && desired < s.CurrentReplicas {
		e.healthyRuns++
		if e.healthyRuns >= 3 {
			clamped := clampReplicas(desired, p.MinReplicas, p.MaxReplicas)
			if maxDown := s.CurrentReplicas - p.MaxScaleDownStep; clamped < maxDown {
				clamped = maxDown
			}
			if clamped < s.CurrentReplicas {
				rec.Decision = model.DecisionScaleDown
				rec.RecommendedReplicas = clamped
				rec.Confidence = 0.7
				rec.Reason = fmt.Sprintf(
					"Sustained low load: request rate %.1f/s only requires %d replicas at %.1f/s per replica after 3 healthy evaluations.",
					s.RequestRate, baseReplicas, p.TargetRequestsPerReplica,
				)
				return rec
			}
		}
		rec.Decision = model.DecisionHold
		rec.Reason = fmt.Sprintf("Healthy but holding: %d of 3 consecutive healthy evaluations needed before scaling down.", e.healthyRuns)
		return rec
	}

	e.healthyRuns = 0
	rec.Decision = model.DecisionHold
	rec.Reason = "Signals within target; holding at current replica count."
	return rec
}

func clampReplicas(v, min, max int32) int32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// detectPodOutlier flags a pod whose error rate or P95 latency diverges
// sharply from its peers, considering only pods with enough traffic to be
// statistically meaningful.
func detectPodOutlier(pods []model.PodSnapshot, minRequests int, errorRateMultiplier, latencyMultiplier float64) (model.PodSnapshot, bool) {
	eligible := make([]model.PodSnapshot, 0, len(pods))
	for _, p := range pods {
		if p.RequestRate >= float64(minRequests) {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) < 2 {
		return model.PodSnapshot{}, false
	}

	for i, candidate := range eligible {
		var otherErrSum, otherLatSum float64
		otherCount := 0
		for j, other := range eligible {
			if i == j {
				continue
			}
			otherErrSum += other.ErrorRate
			otherLatSum += other.P95MS
			otherCount++
		}
		avgErr := otherErrSum / float64(otherCount)
		avgLatency := otherLatSum / float64(otherCount)

		errBad := avgErr > 0 && candidate.ErrorRate > avgErr*errorRateMultiplier
		latBad := avgLatency > 0 && candidate.P95MS > avgLatency*latencyMultiplier
		if errBad || latBad {
			return candidate, true
		}
	}
	return model.PodSnapshot{}, false
}
