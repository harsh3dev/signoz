// Package controller drives the evaluate-recommend-approve-execute loop: it
// reads current state, asks the policy engine for a recommendation, and only
// ever changes the published desired-replica count through an explicit,
// mode-gated decision (dry-run observes, approval waits for a human,
// automatic executes scaling immediately). Pod remediation is always
// approval-gated, regardless of mode.
package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/kube"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/model"
	"github.com/harsh3dev/signoz/incident-autopilot/internal/policy"
)

// SnapshotProvider is satisfied by signoz.Client.
type SnapshotProvider interface {
	Snapshot(ctx context.Context, cfg config.Config, replicas int32) (model.Snapshot, error)
}

// ReplicaStatus is the minimal Kubernetes replica state the controller needs
// to combine with SigNoz signals.
type ReplicaStatus = model.ReplicaStatus

// ReplicaReader is satisfied by kube.Client.
type ReplicaReader interface {
	Replicas(ctx context.Context) (ReplicaStatus, error)
}

// RolloutVerifier waits for a Deployment rollout to complete.
type RolloutVerifier interface {
	Target(ctx context.Context) (kube.TargetState, error)
	WaitForRollout(ctx context.Context, generation int64, timeout time.Duration) error
}

// Recorder is satisfied by telemetry.Emitter.
type Recorder interface {
	RecordRecommendation(rec model.Recommendation)
	// RecordPublishedReplicas sets the KEDA-facing metric. Callers must only
	// pass the approval-gated published replica count.
	RecordPublishedReplicas(replicas int32)
	// RecordObservedReplicas sets the informational, raw observed count.
	RecordObservedReplicas(replicas int32)
	RecordDecision(rec model.Recommendation)
	RecordIncidentReport(rec model.Recommendation, verification model.Verification, beforeReplicas, afterReplicas int32)
	Heartbeat()
}

// State is persisted atomically to config.Controller.StatePath so a restart
// never re-executes or forgets an in-flight action.
type State struct {
	LastRecommendation model.Recommendation `json:"last_recommendation"`
	LastSnapshot       model.Snapshot       `json:"last_snapshot"`
	LastAction         model.Action         `json:"last_action"`
	LastVerification   model.Verification   `json:"last_verification"`
	PublishedReplicas  int32                `json:"published_replicas"`
}

type Controller struct {
	cfg                config.Config
	engine             *policy.Engine
	snapshots          SnapshotProvider
	replicas           ReplicaReader
	rollout            RolloutVerifier
	recorder           Recorder
	approvalSecret     []byte
	metricsHandler     http.Handler
	prometheusAPIProxy http.Handler

	mu    sync.Mutex
	state State
	now   func() time.Time
	sleep func(time.Duration)
}

type Option func(*Controller)

// WithClock overrides the controller's time source. Tests use this to make
// cooldown and expiry checks deterministic.
func WithClock(now func() time.Time) Option {
	return func(c *Controller) { c.now = now }
}

// WithMetricsHandler mounts a Prometheus exposition handler at GET /metrics.
func WithMetricsHandler(h http.Handler) Option {
	return func(c *Controller) { c.metricsHandler = h }
}

// WithPrometheusAPIHandler mounts a Prometheus-query-API-compatible handler
// at GET /api/v1/query, which is what KEDA's "prometheus" ScaledObject
// trigger actually calls to read autopilot_recommended_replicas.
func WithPrometheusAPIHandler(h http.Handler) Option {
	return func(c *Controller) { c.prometheusAPIProxy = h }
}

// WithRolloutVerifier supplies the Kubernetes rollout tracker used by Verify.
func WithRolloutVerifier(rv RolloutVerifier) Option {
	return func(c *Controller) { c.rollout = rv }
}

// WithSleep overrides the verification window wait (tests only).
func WithSleep(sleep func(time.Duration)) Option {
	return func(c *Controller) { c.sleep = sleep }
}

func New(cfg config.Config, engine *policy.Engine, snapshots SnapshotProvider, replicas ReplicaReader, recorder Recorder, approvalSecret []byte, opts ...Option) (*Controller, error) {
	state, err := loadState(cfg.Controller.StatePath)
	if err != nil {
		return nil, err
	}
	c := &Controller{
		cfg:            cfg,
		engine:         engine,
		snapshots:      snapshots,
		replicas:       replicas,
		recorder:       recorder,
		approvalSecret: approvalSecret,
		state:          state,
		now:            time.Now,
		sleep:          time.Sleep,
	}
	for _, opt := range opts {
		opt(c)
	}
	if rv, ok := replicas.(RolloutVerifier); ok {
		c.rollout = rv
	}
	return c, nil
}

// Verify waits for the post-scale rollout, observes the verification window,
// re-queries SigNoz, and classifies whether the action recovered service health.
func (c *Controller) Verify(ctx context.Context, before model.Snapshot, rec model.Recommendation) (model.Verification, error) {
	if c.rollout == nil {
		return model.Verification{}, fmt.Errorf("rollout verifier is not configured")
	}

	target, err := c.rollout.Target(ctx)
	if err != nil {
		return model.Verification{}, fmt.Errorf("read rollout target: %w", err)
	}

	if err := c.rollout.WaitForRollout(ctx, target.Generation, 0); err != nil {
		verification := model.Verification{
			RecommendationID: rec.ID,
			BeforeSLI:      before.SLI,
			BeforeP95MS:    before.P95MS,
			BeforeErrorRate: before.ErrorRate,
			Result:         "rollout_failed",
		}
		c.recorder.RecordIncidentReport(rec, verification, before.CurrentReplicas, target.DesiredReplicas)
		return verification, nil
	}

	if window := c.cfg.Controller.VerificationWindow; window > 0 {
		if c.sleep != nil {
			c.sleep(window)
		}
	}

	afterTarget, err := c.rollout.Target(ctx)
	if err != nil {
		return model.Verification{}, fmt.Errorf("read post-rollout target: %w", err)
	}

	after, err := c.snapshots.Snapshot(ctx, c.cfg, afterTarget.DesiredReplicas)
	if err != nil {
		return model.Verification{}, fmt.Errorf("post-action snapshot: %w", err)
	}

	verification := model.Verification{
		RecommendationID: rec.ID,
		BeforeSLI:      before.SLI,
		AfterSLI:         after.SLI,
		BeforeP95MS:      before.P95MS,
		AfterP95MS:       after.P95MS,
		BeforeErrorRate:  before.ErrorRate,
		AfterErrorRate:   after.ErrorRate,
		Result:           classifyVerification(before, after, c.cfg.Signals.SLIObjective),
	}
	c.recorder.RecordIncidentReport(rec, verification, before.CurrentReplicas, afterTarget.DesiredReplicas)
	return verification, nil
}

func classifyVerification(before, after model.Snapshot, objective float64) string {
	latencyWorsened := after.P95MS > before.P95MS
	errorsWorsened := after.ErrorRate > before.ErrorRate

	if after.SLI >= objective && !latencyWorsened && !errorsWorsened {
		return "recovered"
	}
	if after.SLI > before.SLI && after.SLI < objective {
		return "improved"
	}
	return "ineffective"
}

// Evaluate runs one iteration of the control loop: read current replicas,
// query trusted telemetry, classify the situation, and either hold, publish
// a scale change (automatic mode), or wait for approval.
func (c *Controller) Evaluate(ctx context.Context) error {
	c.mu.Lock()

	var verifyAfter struct {
		run    bool
		before model.Snapshot
		rec    model.Recommendation
	}

	status, err := c.replicas.Replicas(ctx)
	if err != nil {
		c.recorder.Heartbeat()
		c.mu.Unlock()
		return fmt.Errorf("read current replicas: %w", err)
	}
	replicas := status.Current

	now := c.now()
	c.recorder.RecordObservedReplicas(replicas)

	snapshot, err := c.snapshots.Snapshot(ctx, c.cfg, replicas)
	if err != nil {
		// Untrustworthy telemetry: fail closed by publishing the replica
		// count we can observe directly, and take no other action.
		c.state.PublishedReplicas = replicas
		c.recorder.RecordPublishedReplicas(replicas)
		c.recorder.Heartbeat()
		err = c.persistStateLocked()
		c.mu.Unlock()
		return err
	}
	snapshot.Available = status.Available

	rec := c.engine.Evaluate(snapshot, now)
	c.state.LastRecommendation = rec
	c.state.LastSnapshot = snapshot
	c.recorder.RecordRecommendation(rec)
	c.recorder.RecordDecision(rec)

	if c.state.PublishedReplicas == 0 {
		// Bootstrap: before any action has ever been taken, published
		// replicas mirrors the observed count.
		c.state.PublishedReplicas = replicas
	}

	switch rec.Decision {
	case model.DecisionIndeterminate:
		c.state.PublishedReplicas = replicas
	case model.DecisionScaleUp, model.DecisionScaleDown:
		if c.cfg.Controller.Mode == "automatic" {
			if c.executeLocked(rec, now) {
				verifyAfter.run = true
				verifyAfter.before = snapshot
				verifyAfter.rec = rec
			}
		}
		// dry-run: never publish a scale change.
		// approval: hold until POST /api/actions/{id}/approve.
	}

	c.recorder.RecordPublishedReplicas(c.state.PublishedReplicas)
	c.recorder.Heartbeat()
	if err := c.persistStateLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	if verifyAfter.run {
		if err := c.verifyAfterAction(ctx, verifyAfter.before, verifyAfter.rec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) verifyAfterAction(ctx context.Context, before model.Snapshot, rec model.Recommendation) error {
	if c.rollout == nil {
		return nil
	}
	verification, err := c.Verify(ctx, before, rec)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.LastAction.RecommendationID != rec.ID {
		return err
	}
	if err != nil {
		return fmt.Errorf("post-scale verification: %w", err)
	}
	c.state.LastVerification = verification
	return c.persistStateLocked()
}

func (c *Controller) executeLocked(rec model.Recommendation, now time.Time) bool {
	if c.state.LastAction.RecommendationID == rec.ID && !c.state.LastAction.CompletedAt.IsZero() {
		return false // never execute the same recommendation twice.
	}
	c.state.PublishedReplicas = rec.RecommendedReplicas
	c.state.LastAction = model.Action{
		RecommendationID: rec.ID,
		StartedAt:        now,
		CompletedAt:      now,
		Result:           "published_desired_replicas",
	}
	c.engine.RecordAction(now)
	return true
}

// Approve executes a pending scaling recommendation once its signed token
// verifies, it has not expired, and it has not already been executed.
func (c *Controller) Approve(ctx context.Context, id, token, operator string, now time.Time) error {
	c.mu.Lock()

	rec := c.state.LastRecommendation
	if rec.ID != id {
		c.mu.Unlock()
		return fmt.Errorf("unknown or superseded recommendation %q", id)
	}
	if rec.Decision != model.DecisionScaleUp && rec.Decision != model.DecisionScaleDown {
		c.mu.Unlock()
		return fmt.Errorf("recommendation %q is not an executable scaling action", id)
	}
	if c.state.LastAction.RecommendationID == id && !c.state.LastAction.CompletedAt.IsZero() {
		c.mu.Unlock()
		return nil // idempotent: already approved and executed.
	}
	if err := VerifyApproval(c.approvalSecret, token, id, now); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("verify approval: %w", err)
	}

	beforeSnapshot := c.state.LastSnapshot
	recCopy := rec

	c.state.PublishedReplicas = rec.RecommendedReplicas
	c.state.LastAction = model.Action{
		RecommendationID: id,
		ApprovedBy:       operator,
		ApprovedAt:       now,
		StartedAt:        now,
		CompletedAt:      now,
		Result:           "approved_and_published",
	}
	c.engine.RecordAction(now)
	c.recorder.RecordPublishedReplicas(c.state.PublishedReplicas)
	if err := c.persistStateLocked(); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	return c.verifyAfterAction(ctx, beforeSnapshot, recCopy)
}

func (c *Controller) PublishedReplicas() int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.PublishedReplicas
}

func (c *Controller) PendingRecommendation() model.Recommendation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.LastRecommendation
}

func (c *Controller) persistStateLocked() error {
	if c.cfg.Controller.StatePath == "" {
		return nil
	}
	return writeStateAtomic(c.cfg.Controller.StatePath, c.state)
}

func writeStateAtomic(path string, state State) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".autopilot-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	defer os.Remove(tmp.Name())

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		tmp.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename state file: %w", err)
	}
	return nil
}

func loadState(path string) (State, error) {
	if path == "" {
		return State{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read state file: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("decode state file: %w", err)
	}
	return s, nil
}

// SignApproval produces a self-contained, HMAC-signed token binding a
// recommendation ID to its expiry. VerifyApproval is the only source of
// truth for whether a token has expired.
func SignApproval(secret []byte, id string, expiresAt time.Time) string {
	payload := id + "." + strconv.FormatInt(expiresAt.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func VerifyApproval(secret []byte, token, id string, now time.Time) error {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("malformed approval token")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("malformed approval token payload: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("malformed approval token signature: %w", err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payloadBytes)
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return fmt.Errorf("invalid approval token signature")
	}

	payload := string(payloadBytes)
	sepIdx := strings.LastIndex(payload, ".")
	if sepIdx < 0 {
		return fmt.Errorf("malformed approval token payload")
	}
	payloadID := payload[:sepIdx]
	expUnix, err := strconv.ParseInt(payload[sepIdx+1:], 10, 64)
	if err != nil {
		return fmt.Errorf("malformed approval token expiry: %w", err)
	}
	if payloadID != id {
		return fmt.Errorf("approval token does not match recommendation %q", id)
	}
	if !now.Before(time.Unix(expUnix, 0)) {
		return fmt.Errorf("approval token for %q has expired", id)
	}
	return nil
}

// Router exposes the approval UI, approval API, and (if configured) the
// Prometheus metrics endpoint KEDA scrapes for the desired-replica signal.
func (c *Controller) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /actions/latest", c.handleLatest)
	mux.HandleFunc("GET /actions/{id}", c.handleShowAction)
	mux.HandleFunc("POST /api/actions/{id}/approve", c.handleApprove)
	if c.metricsHandler != nil {
		mux.Handle("GET /metrics", c.metricsHandler)
	}
	if c.prometheusAPIProxy != nil {
		mux.Handle("GET /api/v1/query", c.prometheusAPIProxy)
	}
	return mux
}

func (c *Controller) handleLatest(w http.ResponseWriter, r *http.Request) {
	rec := c.PendingRecommendation()
	if rec.ID == "" {
		http.Error(w, "no recommendation available", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/actions/"+rec.ID, http.StatusFound)
}

const approvalPageTemplate = `<!doctype html>
<html>
<head><meta charset="utf-8"><title>Incident Autopilot: approve action</title></head>
<body>
<h1>Recommendation %s</h1>
<p><strong>Decision:</strong> %s</p>
<p><strong>Current replicas:</strong> %d &rarr; <strong>Recommended replicas:</strong> %d</p>
<p><strong>Reason:</strong> %s</p>
<p><strong>Expires at:</strong> %s</p>
<form method="post" action="/api/actions/%s/approve" onsubmit="return submitApproval(event)">
  <input type="hidden" name="token" value="%s">
  <label>Operator name: <input type="text" name="operator" required></label>
  <button type="submit">Approve</button>
</form>
<script>
function submitApproval(event) {
  event.preventDefault();
  var form = event.target;
  var data = new FormData(form);
  fetch(form.action, {
    method: 'POST',
    headers: {'Authorization': 'Bearer %s', 'X-Autopilot-Operator': data.get('operator')},
    body: new URLSearchParams({token: data.get('token')}),
  }).then(function (res) {
    return res.text();
  }).then(function (body) {
    document.body.insertAdjacentHTML('beforeend', '<pre>' + body + '</pre>');
  });
  return false;
}
</script>
</body>
</html>
`

func (c *Controller) handleShowAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := c.PendingRecommendation()
	if rec.ID != id {
		http.Error(w, "recommendation not found or superseded", http.StatusNotFound)
		return
	}
	token := SignApproval(c.approvalSecret, rec.ID, rec.ExpiresAt)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, approvalPageTemplate,
		html.EscapeString(rec.ID),
		html.EscapeString(string(rec.Decision)),
		rec.CurrentReplicas,
		rec.RecommendedReplicas,
		html.EscapeString(rec.Reason),
		rec.ExpiresAt.Format(time.RFC3339),
		html.EscapeString(rec.ID),
		html.EscapeString(token),
		html.EscapeString(string(c.approvalSecret)),
	)
}

func (c *Controller) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == "" || !hmac.Equal([]byte(bearer), c.approvalSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	operator := r.Header.Get("X-Autopilot-Operator")
	if operator == "" {
		operator = r.FormValue("operator")
	}
	if operator == "" {
		http.Error(w, "missing X-Autopilot-Operator", http.StatusBadRequest)
		return
	}
	token := r.FormValue("token")

	if err := c.Approve(r.Context(), id, token, operator, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "approved", "id": id})
}
