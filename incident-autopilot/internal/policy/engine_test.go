package policy

import (
	"testing"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
)

func baseTestConfig() config.Config {
	var cfg config.Config
	cfg.Policy.MinReplicas = 1
	cfg.Policy.MaxReplicas = 10
	cfg.Policy.TargetRequestsPerReplica = 25
	cfg.Policy.LatencyTargetMS = 800
	cfg.Policy.ErrorRateTarget = 0.02
	cfg.Policy.MaxScaleUpStep = 4
	cfg.Policy.MaxScaleDownStep = 1
	cfg.Policy.Cooldown = 2 * time.Minute
	cfg.Policy.PodOutlier.MinimumRequests = 20
	cfg.Policy.PodOutlier.ErrorRateMultiplier = 3
	cfg.Policy.PodOutlier.LatencyMultiplier = 2
	cfg.Policy.PodOutlier.ConsecutiveEvaluations = 2
	cfg.Signals.SLIObjective = 0.99
	return cfg
}

func TestCapacityPressureRecommendsBoundedScaleUp(t *testing.T) {
	cfg := baseTestConfig()
	engine := New(cfg)

	snapshot := model.Snapshot{
		CurrentReplicas: 2,
		Available:       2,
		RequestRate:     140,
		P95MS:           1200,
		ErrorRate:       0.08,
		SLI:             0.92,
	}

	rec := engine.Evaluate(snapshot, time.Now())

	if rec.Decision != model.DecisionScaleUp {
		t.Fatalf("expected scale_up, got %s (reason: %s)", rec.Decision, rec.Reason)
	}
	if rec.RecommendedReplicas != 6 {
		t.Fatalf("expected 6 replicas, got %d", rec.RecommendedReplicas)
	}
	expectedReason := "Capacity pressure: request rate 140.0/s requires 6 replicas at 25.0/s per replica; " +
		"P95 latency 1200ms exceeds 800ms; SLI 92.00% is below 99.00%."
	if rec.Reason != expectedReason {
		t.Fatalf("expected reason:\n%q\ngot:\n%q", expectedReason, rec.Reason)
	}
}

func TestLowTrafficErrorsRecommendInvestigation(t *testing.T) {
	cfg := baseTestConfig()
	engine := New(cfg)

	snapshot := model.Snapshot{
		CurrentReplicas: 2,
		Available:       2,
		RequestRate:     5,
		P95MS:           100,
		ErrorRate:       0.3,
		SLI:             0.7,
	}

	rec := engine.Evaluate(snapshot, time.Now())

	if rec.Decision != model.DecisionInvestigate {
		t.Fatalf("expected investigate, got %s (reason: %s)", rec.Decision, rec.Reason)
	}
	if rec.RecommendedReplicas != snapshot.CurrentReplicas {
		t.Fatalf("expected replicas to stay at %d, got %d", snapshot.CurrentReplicas, rec.RecommendedReplicas)
	}
}

func TestOnePodOutlierRecommendsQuarantine(t *testing.T) {
	cfg := baseTestConfig()
	engine := New(cfg)

	snapshot := model.Snapshot{
		CurrentReplicas: 3,
		Available:       3,
		RequestRate:     150,
		P95MS:           300,
		ErrorRate:       0.05,
		SLI:             0.95,
		Pods: []model.PodSnapshot{
			{Name: "checkout-api-aaa", Ready: true, RequestRate: 50, ErrorRate: 0.01, P95MS: 100},
			{Name: "checkout-api-bbb", Ready: true, RequestRate: 50, ErrorRate: 0.01, P95MS: 100},
			{Name: "checkout-api-ccc", Ready: true, RequestRate: 50, ErrorRate: 0.5, P95MS: 1500},
		},
	}
	now := time.Now()

	first := engine.Evaluate(snapshot, now)
	if first.Decision == model.DecisionQuarantine {
		t.Fatalf("did not expect quarantine on the first evaluation, got %s", first.Reason)
	}

	second := engine.Evaluate(snapshot, now.Add(15*time.Second))
	if second.Decision != model.DecisionQuarantine {
		t.Fatalf("expected quarantine on the second consecutive evaluation, got %s (reason: %s)", second.Decision, second.Reason)
	}
	if second.TargetPod != "checkout-api-ccc" {
		t.Fatalf("expected target pod checkout-api-ccc, got %q", second.TargetPod)
	}
}

func TestMissingAvailabilityReturnsIndeterminate(t *testing.T) {
	cfg := baseTestConfig()
	engine := New(cfg)

	snapshot := model.Snapshot{
		CurrentReplicas: 2,
		Available:       0,
		RequestRate:     50,
	}

	rec := engine.Evaluate(snapshot, time.Now())

	if rec.Decision != model.DecisionIndeterminate {
		t.Fatalf("expected indeterminate, got %s", rec.Decision)
	}
}

func TestCooldownReturnsHold(t *testing.T) {
	cfg := baseTestConfig()
	engine := New(cfg)
	now := time.Now()
	engine.RecordAction(now)

	snapshot := model.Snapshot{
		CurrentReplicas: 2,
		Available:       2,
		RequestRate:     140,
		P95MS:           1200,
		ErrorRate:       0.08,
		SLI:             0.92,
	}

	rec := engine.Evaluate(snapshot, now.Add(30*time.Second))

	if rec.Decision != model.DecisionHold {
		t.Fatalf("expected hold during cooldown, got %s (reason: %s)", rec.Decision, rec.Reason)
	}
	if rec.RecommendedReplicas != snapshot.CurrentReplicas {
		t.Fatalf("expected replicas to stay at %d during cooldown, got %d", snapshot.CurrentReplicas, rec.RecommendedReplicas)
	}
}

func TestScaleDownIsConservative(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Policy.Cooldown = 0
	engine := New(cfg)
	now := time.Now()

	snapshot := model.Snapshot{
		CurrentReplicas: 4,
		Available:       4,
		RequestRate:     20,
		P95MS:           100,
		ErrorRate:       0.001,
		SLI:             0.999,
	}

	first := engine.Evaluate(snapshot, now)
	if first.Decision != model.DecisionHold {
		t.Fatalf("expected hold on first healthy evaluation, got %s", first.Decision)
	}

	second := engine.Evaluate(snapshot, now.Add(time.Minute))
	if second.Decision != model.DecisionHold {
		t.Fatalf("expected hold on second healthy evaluation, got %s", second.Decision)
	}

	third := engine.Evaluate(snapshot, now.Add(2*time.Minute))
	if third.Decision != model.DecisionScaleDown {
		t.Fatalf("expected scale_down on third consecutive healthy evaluation, got %s (reason: %s)", third.Decision, third.Reason)
	}
	if third.RecommendedReplicas != 3 {
		t.Fatalf("expected conservative scale-down to 3 replicas (max step 1), got %d", third.RecommendedReplicas)
	}
}
