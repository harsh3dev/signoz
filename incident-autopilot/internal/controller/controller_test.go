package controller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/policy"
)

var errSnapshotUnavailable = errors.New("snapshot unavailable")

type fakeReplicas struct {
	n   int32
	err error
}

func (f fakeReplicas) Replicas(context.Context) (ReplicaStatus, error) {
	return ReplicaStatus{Current: f.n, Available: f.n}, f.err
}

// fakeSnapshots always reports capacity pressure numbers (or a fixed error)
// so the policy engine deterministically recommends a scale-up.
type fakeSnapshots struct {
	err error
}

func (f fakeSnapshots) Snapshot(_ context.Context, _ config.Config, replicas int32) (model.Snapshot, error) {
	if f.err != nil {
		return model.Snapshot{}, f.err
	}
	return model.Snapshot{
		CurrentReplicas: replicas,
		RequestRate:     140,
		P95MS:           1200,
		ErrorRate:       0.08,
		SLI:             0.92,
	}, nil
}

type noopRecorder struct{}

func (noopRecorder) RecordRecommendation(model.Recommendation) {}
func (noopRecorder) RecordPublishedReplicas(int32)             {}
func (noopRecorder) RecordObservedReplicas(int32)              {}
func (noopRecorder) RecordDecision(model.Recommendation)       {}
func (noopRecorder) Heartbeat()                                {}

func testConfig(t *testing.T, mode string) config.Config {
	t.Helper()
	var cfg config.Config
	cfg.Target.Service = "checkout-api"
	cfg.Target.Namespace = "autopilot-demo"
	cfg.Target.Deployment = "checkout-api"
	cfg.Policy.MinReplicas = 1
	cfg.Policy.MaxReplicas = 10
	cfg.Policy.TargetRequestsPerReplica = 25
	cfg.Policy.LatencyTargetMS = 800
	cfg.Policy.ErrorRateTarget = 0.02
	cfg.Policy.MaxScaleUpStep = 4
	cfg.Policy.MaxScaleDownStep = 1
	cfg.Policy.PodOutlier.MinimumRequests = 20
	cfg.Policy.PodOutlier.ErrorRateMultiplier = 3
	cfg.Policy.PodOutlier.LatencyMultiplier = 2
	cfg.Policy.PodOutlier.ConsecutiveEvaluations = 2
	cfg.Signals.SLIObjective = 0.99
	cfg.Controller.Mode = mode
	cfg.Controller.StatePath = filepath.Join(t.TempDir(), "state.json")
	return cfg
}

func TestDryRunNeverPublishesScaleChange(t *testing.T) {
	cfg := testConfig(t, "dry-run")
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{}, fakeReplicas{n: 2}, noopRecorder{}, []byte("secret"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}

	rec := c.PendingRecommendation()
	if rec.Decision != model.DecisionScaleUp {
		t.Fatalf("expected the engine to recommend scale_up, got %s", rec.Decision)
	}
	if got := c.PublishedReplicas(); got != 2 {
		t.Fatalf("dry-run must never publish a scale change; expected 2, got %d", got)
	}
}

func TestApprovalModeHoldsUntilApproved(t *testing.T) {
	cfg := testConfig(t, "approval")
	secret := []byte("secret")
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{}, fakeReplicas{n: 2}, noopRecorder{}, secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}
	if got := c.PublishedReplicas(); got != 2 {
		t.Fatalf("approval mode must hold until approved; expected 2, got %d", got)
	}

	rec := c.PendingRecommendation()
	now := rec.CreatedAt.Add(time.Second)
	token := SignApproval(secret, rec.ID, rec.ExpiresAt)
	if err := c.Approve(context.Background(), rec.ID, token, "alice", now); err != nil {
		t.Fatalf("unexpected approve error: %v", err)
	}
	if got := c.PublishedReplicas(); got != rec.RecommendedReplicas {
		t.Fatalf("expected published replicas %d after approval, got %d", rec.RecommendedReplicas, got)
	}
}

func TestExpiredApprovalCannotExecute(t *testing.T) {
	cfg := testConfig(t, "approval")
	secret := []byte("secret")
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{}, fakeReplicas{n: 2}, noopRecorder{}, secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}

	rec := c.PendingRecommendation()
	token := SignApproval(secret, rec.ID, rec.ExpiresAt)
	after := rec.ExpiresAt.Add(time.Minute)

	if err := c.Approve(context.Background(), rec.ID, token, "alice", after); err == nil {
		t.Fatal("expected an error approving an expired recommendation")
	}
	if got := c.PublishedReplicas(); got != 2 {
		t.Fatalf("expired approval must not change published replicas; expected 2, got %d", got)
	}
}

func TestApprovalIsIdempotent(t *testing.T) {
	cfg := testConfig(t, "approval")
	secret := []byte("secret")
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{}, fakeReplicas{n: 2}, noopRecorder{}, secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}

	rec := c.PendingRecommendation()
	now := rec.CreatedAt.Add(time.Second)
	token := SignApproval(secret, rec.ID, rec.ExpiresAt)

	if err := c.Approve(context.Background(), rec.ID, token, "alice", now); err != nil {
		t.Fatalf("unexpected error on first approval: %v", err)
	}
	firstPublished := c.PublishedReplicas()

	if err := c.Approve(context.Background(), rec.ID, token, "bob", now.Add(time.Second)); err != nil {
		t.Fatalf("expected idempotent re-approval to succeed without error, got: %v", err)
	}
	if got := c.PublishedReplicas(); got != firstPublished {
		t.Fatalf("expected published replicas to stay at %d after repeat approval, got %d", firstPublished, got)
	}
}

func TestIndeterminateSnapshotPublishesCurrentReplicas(t *testing.T) {
	cfg := testConfig(t, "approval")
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{err: errSnapshotUnavailable}, fakeReplicas{n: 3}, noopRecorder{}, []byte("secret"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}
	if got := c.PublishedReplicas(); got != 3 {
		t.Fatalf("expected published replicas to fail closed to observed count 3, got %d", got)
	}
}
