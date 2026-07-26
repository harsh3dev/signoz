package controller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/kube"
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
func (noopRecorder) RecordIncidentReport(model.Recommendation, model.Verification, int32, int32) {
}
func (noopRecorder) Heartbeat() {}

type recordingRecorder struct {
	reports []model.Verification
}

func (r *recordingRecorder) RecordRecommendation(model.Recommendation) {}
func (r *recordingRecorder) RecordPublishedReplicas(int32)             {}
func (r *recordingRecorder) RecordObservedReplicas(int32)              {}
func (r *recordingRecorder) RecordDecision(model.Recommendation)       {}
func (r *recordingRecorder) Heartbeat()                                {}

func (r *recordingRecorder) RecordIncidentReport(_ model.Recommendation, verification model.Verification, _, _ int32) {
	r.reports = append(r.reports, verification)
}

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

	if err := c.Evaluate(context.Background()); err == nil {
		t.Fatal("expected snapshot error when telemetry is unavailable")
	}
	if got := c.PublishedReplicas(); got != 3 {
		t.Fatalf("expected published replicas to fail closed to observed count 3, got %d", got)
	}
}

type sequenceSnapshots struct {
	values []model.Snapshot
	err    error
	calls  int
}

func (s *sequenceSnapshots) Snapshot(_ context.Context, _ config.Config, replicas int32) (model.Snapshot, error) {
	if s.err != nil {
		return model.Snapshot{}, s.err
	}
	idx := s.calls
	s.calls++
	if idx >= len(s.values) {
		idx = len(s.values) - 1
	}
	snap := s.values[idx]
	snap.CurrentReplicas = replicas
	return snap, nil
}

type fakeRollout struct {
	target      kube.TargetState
	waitErr     error
	targetCalls int
}

func (f *fakeRollout) Target(context.Context) (kube.TargetState, error) {
	f.targetCalls++
	return f.target, nil
}

func (f *fakeRollout) WaitForRollout(context.Context, int64, time.Duration) error {
	return f.waitErr
}

func TestVerificationMarksRecoveredWhenSLIMeetsObjective(t *testing.T) {
	cfg := testConfig(t, "automatic")
	cfg.Signals.SLIObjective = 0.99
	cfg.Controller.VerificationWindow = 0

	before := model.Snapshot{
		CurrentReplicas: 2,
		SLI:             0.92,
		P95MS:           1200,
		ErrorRate:       0.08,
	}
	rec := model.Recommendation{
		ID:                  "rec-1",
		Decision:            model.DecisionScaleUp,
		CurrentReplicas:     2,
		RecommendedReplicas: 6,
	}

	snapshots := &sequenceSnapshots{values: []model.Snapshot{
		{SLI: 0.995, P95MS: 800, ErrorRate: 0.01},
	}}
	rollout := &fakeRollout{target: kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 6}}

	c, err := New(cfg, policy.New(cfg), snapshots, fakeReplicas{n: 2}, noopRecorder{}, []byte("secret"),
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verification, err := c.Verify(context.Background(), before, rec)
	if err != nil {
		t.Fatalf("unexpected verify error: %v", err)
	}
	if verification.Result != "recovered" {
		t.Fatalf("expected recovered, got %s", verification.Result)
	}
	if verification.AfterSLI != 0.995 {
		t.Fatalf("expected after SLI 0.995, got %f", verification.AfterSLI)
	}
}

func TestVerificationMarksIneffectiveWhenSLIStillFails(t *testing.T) {
	cfg := testConfig(t, "automatic")
	cfg.Signals.SLIObjective = 0.99
	cfg.Controller.VerificationWindow = 0

	before := model.Snapshot{
		CurrentReplicas: 2,
		SLI:             0.92,
		P95MS:           1200,
		ErrorRate:       0.08,
	}
	rec := model.Recommendation{
		ID:                  "rec-2",
		Decision:            model.DecisionScaleUp,
		CurrentReplicas:     2,
		RecommendedReplicas: 6,
	}

	snapshots := &sequenceSnapshots{values: []model.Snapshot{
		{SLI: 0.90, P95MS: 1300, ErrorRate: 0.09},
	}}
	rollout := &fakeRollout{target: kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 6}}

	c, err := New(cfg, policy.New(cfg), snapshots, fakeReplicas{n: 2}, noopRecorder{}, []byte("secret"),
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verification, err := c.Verify(context.Background(), before, rec)
	if err != nil {
		t.Fatalf("unexpected verify error: %v", err)
	}
	if verification.Result != "ineffective" {
		t.Fatalf("expected ineffective, got %s", verification.Result)
	}
}

func TestClassifyVerificationImproved(t *testing.T) {
	before := model.Snapshot{SLI: 0.92, P95MS: 1200, ErrorRate: 0.08}
	after := model.Snapshot{SLI: 0.96, P95MS: 900, ErrorRate: 0.03}
	if got := classifyVerification(before, after, 0.99); got != "improved" {
		t.Fatalf("expected improved, got %s", got)
	}
}

func TestVerificationRolloutFailed(t *testing.T) {
	cfg := testConfig(t, "automatic")
	cfg.Controller.VerificationWindow = 0

	before := model.Snapshot{
		CurrentReplicas: 2,
		SLI:             0.92,
		P95MS:           1200,
		ErrorRate:       0.08,
	}
	rec := model.Recommendation{
		ID:                  "rec-rollout-failed",
		Decision:            model.DecisionScaleUp,
		CurrentReplicas:     2,
		RecommendedReplicas: 6,
	}

	recorder := &recordingRecorder{}
	rollout := &fakeRollout{
		target:  kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 1},
		waitErr: errors.New("rollout timed out"),
	}

	c, err := New(cfg, policy.New(cfg), &sequenceSnapshots{}, fakeReplicas{n: 2}, recorder, []byte("secret"),
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	verification, err := c.Verify(context.Background(), before, rec)
	if err != nil {
		t.Fatalf("unexpected verify error: %v", err)
	}
	if verification.Result != "rollout_failed" {
		t.Fatalf("expected rollout_failed, got %s", verification.Result)
	}
	if len(recorder.reports) != 1 {
		t.Fatalf("expected one incident report, got %d", len(recorder.reports))
	}
	if recorder.reports[0].Result != "rollout_failed" {
		t.Fatalf("expected incident report rollout_failed, got %s", recorder.reports[0].Result)
	}
}

func TestAutomaticModeVerifiesAfterScale(t *testing.T) {
	cfg := testConfig(t, "automatic")
	cfg.Controller.VerificationWindow = 0

	recorder := &recordingRecorder{}
	snapshots := &sequenceSnapshots{values: []model.Snapshot{
		{RequestRate: 140, P95MS: 1200, ErrorRate: 0.08, SLI: 0.92, Available: 2},
		{SLI: 0.995, P95MS: 800, ErrorRate: 0.01},
	}}
	rollout := &fakeRollout{target: kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 6}}

	c, err := New(cfg, policy.New(cfg), snapshots, fakeReplicas{n: 2}, recorder, []byte("secret"),
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}
	if got := c.PublishedReplicas(); got != 6 {
		t.Fatalf("expected published replicas 6, got %d", got)
	}
	if len(recorder.reports) != 1 {
		t.Fatalf("expected one incident report after automatic scale, got %d", len(recorder.reports))
	}
	if recorder.reports[0].Result != "recovered" {
		t.Fatalf("expected recovered verification, got %s", recorder.reports[0].Result)
	}
}

func TestApproveVerifiesAfterScale(t *testing.T) {
	cfg := testConfig(t, "approval")
	cfg.Controller.VerificationWindow = 0
	secret := []byte("secret")

	recorder := &recordingRecorder{}
	snapshots := &sequenceSnapshots{values: []model.Snapshot{
		{RequestRate: 140, P95MS: 1200, ErrorRate: 0.08, SLI: 0.92, Available: 2},
		{SLI: 0.995, P95MS: 800, ErrorRate: 0.01},
	}}
	rollout := &fakeRollout{target: kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 6}}

	c, err := New(cfg, policy.New(cfg), snapshots, fakeReplicas{n: 2}, recorder, secret,
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := c.Evaluate(context.Background()); err != nil {
		t.Fatalf("unexpected evaluate error: %v", err)
	}
	if len(recorder.reports) != 0 {
		t.Fatalf("expected no incident report before approval, got %d", len(recorder.reports))
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
	if len(recorder.reports) != 1 {
		t.Fatalf("expected one incident report after approval, got %d", len(recorder.reports))
	}
	if recorder.reports[0].Result != "recovered" {
		t.Fatalf("expected recovered verification, got %s", recorder.reports[0].Result)
	}
}

func TestApproveVerifiesRolloutFailed(t *testing.T) {
	cfg := testConfig(t, "approval")
	cfg.Controller.VerificationWindow = 0
	secret := []byte("secret")

	recorder := &recordingRecorder{}
	snapshots := &sequenceSnapshots{values: []model.Snapshot{
		{RequestRate: 140, P95MS: 1200, ErrorRate: 0.08, SLI: 0.92, Available: 2},
	}}
	rollout := &fakeRollout{
		target:  kube.TargetState{Generation: 2, DesiredReplicas: 6, AvailableReplicas: 1},
		waitErr: errors.New("rollout timed out"),
	}

	c, err := New(cfg, policy.New(cfg), snapshots, fakeReplicas{n: 2}, recorder, secret,
		WithRolloutVerifier(rollout),
		WithSleep(func(time.Duration) {}),
	)
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
		t.Fatalf("unexpected approve error: %v", err)
	}
	if len(recorder.reports) != 1 {
		t.Fatalf("expected one incident report after failed rollout, got %d", len(recorder.reports))
	}
	if recorder.reports[0].Result != "rollout_failed" {
		t.Fatalf("expected rollout_failed verification, got %s", recorder.reports[0].Result)
	}
}

type fakeRemediator struct {
	target             kube.TargetState
	setReadyCalls      int
	waitRoutedCalls    int
	waitReplaceCalls   int
	deleteCalls        int
	waitReplaceErr     error
	deleteErr          error
	syncCalls          int
}

func (f *fakeRemediator) SetAutopilotReady(context.Context, string, types.UID, bool, string, string) error {
	f.setReadyCalls++
	return nil
}

func (f *fakeRemediator) WaitUntilNotRouted(context.Context, types.UID, time.Duration) error {
	f.waitRoutedCalls++
	return nil
}

func (f *fakeRemediator) DeleteOwnedPod(context.Context, string, types.UID) error {
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeRemediator) WaitForReplacementReady(context.Context, types.UID, map[types.UID]struct{}, time.Duration) error {
	f.waitReplaceCalls++
	return f.waitReplaceErr
}

func (f *fakeRemediator) SyncReadinessGates(context.Context, string) error {
	f.syncCalls++
	return nil
}

func (f *fakeRemediator) Target(context.Context) (kube.TargetState, error) {
	return f.target, nil
}

func (f *fakeRemediator) WaitForRollout(context.Context, int64, time.Duration) error {
	return nil
}

func quarantineRecommendation(id, targetPod string, current, recommended int32) model.Recommendation {
	now := time.Unix(1_700_000_000, 0)
	return model.Recommendation{
		ID:                  id,
		CreatedAt:           now,
		ExpiresAt:           now.Add(5 * time.Minute),
		Decision:            model.DecisionQuarantine,
		CurrentReplicas:     current,
		RecommendedReplicas: recommended,
		TargetPod:           targetPod,
		Reason:              "Pod outlier detected",
	}
}

func TestExpiredPodApprovalDoesNothing(t *testing.T) {
	cfg := testConfig(t, "approval")
	secret := []byte("secret")
	podUID := types.UID("pod-uid-expired")

	remediator := &fakeRemediator{
		target: kube.TargetState{
			Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-bad", UID: podUID}}},
		},
	}
	c, err := New(cfg, policy.New(cfg), fakeSnapshots{}, fakeReplicas{n: 3}, noopRecorder{}, secret,
		WithRolloutVerifier(remediator),
		WithPodRemediator(remediator),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := quarantineRecommendation("rec-quarantine-expired", "checkout-api-bad", 3, 4)
	c.mu.Lock()
	c.state.LastRecommendation = rec
	c.state.PublishedReplicas = 3
	c.mu.Unlock()

	token := SignApproval(secret, rec.ID, rec.ExpiresAt)
	after := rec.ExpiresAt.Add(time.Minute)
	if err := c.Approve(context.Background(), rec.ID, token, "alice", after); err == nil {
		t.Fatal("expected an error approving an expired quarantine recommendation")
	}
	if remediator.setReadyCalls != 0 {
		t.Fatalf("expected no quarantine actions, got %d SetAutopilotReady calls", remediator.setReadyCalls)
	}
	if got := c.PublishedReplicas(); got != 3 {
		t.Fatalf("expired quarantine approval must not change published replicas; expected 3, got %d", got)
	}
}

func TestReplacementFailurePreservesQuarantinedPod(t *testing.T) {
	cfg := testConfig(t, "approval")
	cfg.Controller.VerificationWindow = 0
	secret := []byte("secret")
	podUID := types.UID("pod-uid-preserve")

	remediator := &fakeRemediator{
		target: kube.TargetState{
			Generation:        2,
			DesiredReplicas:   4,
			AvailableReplicas: 4,
			Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-bad", UID: podUID}}},
		},
		waitReplaceErr: errors.New("replacement pod not ready"),
	}
	c, err := New(cfg, policy.New(cfg), &sequenceSnapshots{values: []model.Snapshot{
		{SLI: 0.995, P95MS: 800, ErrorRate: 0.01},
	}}, fakeReplicas{n: 3}, noopRecorder{}, secret,
		WithRolloutVerifier(remediator),
		WithPodRemediator(remediator),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := quarantineRecommendation("rec-quarantine-preserve", "checkout-api-bad", 3, 4)
	c.mu.Lock()
	c.state.LastRecommendation = rec
	c.state.LastSnapshot = model.Snapshot{CurrentReplicas: 3, SLI: 0.95}
	c.state.PublishedReplicas = 3
	c.mu.Unlock()

	token := SignApproval(secret, rec.ID, rec.ExpiresAt)
	now := rec.CreatedAt.Add(time.Second)
	if err := c.Approve(context.Background(), rec.ID, token, "alice", now); err == nil {
		t.Fatal("expected replacement failure to return an error")
	}
	if remediator.setReadyCalls != 1 {
		t.Fatalf("expected pod to be quarantined once, got %d calls", remediator.setReadyCalls)
	}
	if remediator.deleteCalls != 0 {
		t.Fatalf("expected quarantined pod to be preserved, got %d delete calls", remediator.deleteCalls)
	}
	if got := c.PublishedReplicas(); got != 4 {
		t.Fatalf("expected replacement capacity to be published as 4, got %d", got)
	}
}

func TestApproveQuarantineReplacesPod(t *testing.T) {
	cfg := testConfig(t, "approval")
	cfg.Controller.VerificationWindow = 0
	secret := []byte("secret")
	podUID := types.UID("pod-uid-replace")

	recorder := &recordingRecorder{}
	remediator := &fakeRemediator{
		target: kube.TargetState{
			Generation:        2,
			DesiredReplicas:   4,
			AvailableReplicas: 4,
			Pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-bad", UID: podUID}}},
		},
	}
	c, err := New(cfg, policy.New(cfg), &sequenceSnapshots{values: []model.Snapshot{
		{SLI: 0.995, P95MS: 800, ErrorRate: 0.01},
	}}, fakeReplicas{n: 3}, recorder, secret,
		WithRolloutVerifier(remediator),
		WithPodRemediator(remediator),
		WithSleep(func(time.Duration) {}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := quarantineRecommendation("rec-quarantine-ok", "checkout-api-bad", 3, 4)
	c.mu.Lock()
	c.state.LastRecommendation = rec
	c.state.LastSnapshot = model.Snapshot{CurrentReplicas: 3, SLI: 0.95}
	c.state.PublishedReplicas = 3
	c.mu.Unlock()

	token := SignApproval(secret, rec.ID, rec.ExpiresAt)
	now := rec.CreatedAt.Add(time.Second)
	if err := c.Approve(context.Background(), rec.ID, token, "alice", now); err != nil {
		t.Fatalf("unexpected approve error: %v", err)
	}
	if remediator.deleteCalls != 1 {
		t.Fatalf("expected quarantined pod to be deleted, got %d delete calls", remediator.deleteCalls)
	}
	if got := c.PublishedReplicas(); got != 4 {
		t.Fatalf("expected published replicas 4 during replacement, got %d", got)
	}
	if len(recorder.reports) != 1 {
		t.Fatalf("expected one incident report after quarantine replacement, got %d", len(recorder.reports))
	}
}
