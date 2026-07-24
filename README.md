# SigNoz Incident Autopilot MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a reproducible hackathon demo where SigNoz telemetry drives an explained, approved KEDA scale-up, verifies SLI recovery, and supports approval-based quarantine and replacement of one unhealthy pod.

**Architecture:** A standalone Go controller queries stock SigNoz and Kubernetes, classifies service-wide capacity pressure versus localized pod failure, and publishes a bounded desired-replica metric for KEDA. It emits its decisions and verification results back to SigNoz through OTLP, exposes a small approval API, and installs a native SigNoz dashboard and alerts without modifying SigNoz.

**Tech Stack:** Go 1.25+, OpenTelemetry Go, Prometheus client, Kubernetes `client-go`, KEDA Prometheus scaler, Node.js 20 demo service, Kind, Docker, stock self-hosted SigNoz.

## Global Constraints

- Do not modify SigNoz source code.
- Support one Kubernetes Deployment in one namespace.
- Use deterministic policy rules; no ML or LLM is required for decisions.
- Default to `dry-run`; scaling can use `approval` or `automatic`.
- Pod quarantine and replacement is always approval-only.
- Never scale or quarantine using stale, missing, or contradictory telemetry.
- Never execute arbitrary commands inside containers.
- Every action must have a stable ID, bounded expiry, persisted state, and an audit record.
- Human notifications go through a SigNoz notification channel.
- Scaling decisions query SigNoz directly; SigNoz alerts do not trigger KEDA.
- Use Kubernetes readiness gates and EndpointSlice verification before deleting a pod.
- Do not exceed configured minimum, maximum, or per-action replica limits.

## Hackathon schedule

| Hours | Deliverable |
| --- | --- |
| 0–4 | Go module, configuration, domain model, Kind/KEDA prerequisites |
| 4–10 | Kubernetes demo deployment and deterministic traffic/chaos controls |
| 10–18 | SigNoz queries, trust gate, correlation, explained recommendation |
| 18–26 | Approval API, Prometheus metric, KEDA scale-up |
| 26–34 | Rollout tracking, SLI verification, incident report |
| 34–40 | Localized bad-pod diagnosis and approved replacement |
| 40–48 | Dashboard, alerts, runbook, tests, and backup demo recording |

## Planned file structure

```text
incident-autopilot/
  go.mod
  go.sum
  cmd/autopilot/main.go
  internal/config/config.go
  internal/config/config_test.go
  internal/model/types.go
  internal/signoz/client.go
  internal/signoz/client_test.go
  internal/policy/engine.go
  internal/policy/engine_test.go
  internal/kube/client.go
  internal/kube/client_test.go
  internal/controller/controller.go
  internal/controller/controller_test.go
  internal/telemetry/emitter.go
  internal/telemetry/http.go
  internal/installer/signoz.go
  internal/installer/signoz_test.go
  deploy/autopilot.yaml
  deploy/rbac.yaml
  deploy/scaledobject.yaml
  deploy/demo-app.yaml
  deploy/load-generator.yaml
  deploy/kind.yaml
  scripts/demo-capacity.sh
  scripts/demo-bad-pod.sh
  config.example.yaml
  Dockerfile
  Makefile
  DEMO.md
demo-app/
  Dockerfile
  src/app.js
  src/instrumentation.js
```

---

### Task 1: Controller foundation, configuration, and domain contracts

**Files:**
- Create: `incident-autopilot/go.mod`
- Create: `incident-autopilot/cmd/autopilot/main.go`
- Create: `incident-autopilot/internal/config/config.go`
- Create: `incident-autopilot/internal/config/config_test.go`
- Create: `incident-autopilot/internal/model/types.go`
- Create: `incident-autopilot/config.example.yaml`
- Create: `incident-autopilot/Makefile`

**Interfaces:**
- Produces: `config.Load(path string) (config.Config, error)`
- Produces: `model.Snapshot`, `model.PodSnapshot`, `model.Recommendation`, `model.Action`, and `model.Verification`
- Produces: validated operating modes `dry-run`, `approval`, and `automatic`

- [x] **Step 1: Initialize the Go module**

Create `incident-autopilot/go.mod`:

```go
module github.com/guruvedhanth-s/signoz/incident-autopilot

go 1.25
```

Run:

```bash
cd incident-autopilot
go get github.com/prometheus/client_golang@latest
go get go.opentelemetry.io/otel@latest
go get go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp@latest
go get go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp@latest
go get go.opentelemetry.io/otel/sdk@latest
go get go.opentelemetry.io/otel/sdk/log@latest
go get gopkg.in/yaml.v3@latest
go get k8s.io/api@latest
go get k8s.io/apimachinery@latest
go get k8s.io/client-go@latest
go mod tidy
```

Expected: dependencies resolve and `go.sum` is created.

- [x] **Step 2: Define the domain model**

Create `incident-autopilot/internal/model/types.go`:

```go
package model

import "time"

type Decision string

const (
	DecisionHold          Decision = "hold"
	DecisionScaleUp       Decision = "scale_up"
	DecisionScaleDown     Decision = "scale_down"
	DecisionQuarantine    Decision = "quarantine_replace"
	DecisionInvestigate   Decision = "investigate"
	DecisionIndeterminate Decision = "indeterminate"
)

type PodSnapshot struct {
	Name        string
	UID         string
	Ready       bool
	RequestRate float64
	P95MS       float64
	ErrorRate   float64
	Restarts    int32
}

type Snapshot struct {
	Service         string
	ObservedAt      time.Time
	CurrentReplicas int32
	Available       int32
	RequestRate     float64
	P95MS           float64
	ErrorRate       float64
	SLI             float64
	Pods            []PodSnapshot
}

type Evidence struct {
	Signal   string  `json:"signal"`
	Observed float64 `json:"observed"`
	Target   float64 `json:"target"`
	Summary  string  `json:"summary"`
}

type Recommendation struct {
	ID                  string
	CreatedAt           time.Time
	ExpiresAt           time.Time
	Decision            Decision
	CurrentReplicas     int32
	RecommendedReplicas int32
	TargetPod            string
	Reason               string
	Confidence           float64
	Evidence             []Evidence
	PolicyVersion        string
}

type Action struct {
	RecommendationID string
	ApprovedBy       string
	ApprovedAt       time.Time
	StartedAt        time.Time
	CompletedAt      time.Time
	Result           string
}

type Verification struct {
	RecommendationID string
	BeforeSLI        float64
	AfterSLI         float64
	BeforeP95MS      float64
	AfterP95MS       float64
	BeforeErrorRate  float64
	AfterErrorRate   float64
	Result           string
}
```

- [x] **Step 3: Write failing configuration tests**

Create `incident-autopilot/internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesReplicaBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
policy:
  min_replicas: 6
  max_replicas: 2
controller:
  mode: dry-run
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected invalid replica bounds")
	}
}

func TestLoadRejectsUnknownMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
signoz:
  url: http://signoz:8080
target:
  service: checkout-api
  namespace: demo
  deployment: checkout-api
policy:
  min_replicas: 2
  max_replicas: 10
controller:
  mode: unsafe
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	_, err = Load(path)
	if err == nil {
		t.Fatal("expected unknown mode error")
	}
}
```

- [x] **Step 4: Run tests to verify they fail**

Run:

```bash
go test ./internal/config -v
```

Expected: FAIL because `Config` and `Load` do not exist.

- [x] **Step 5: Implement configuration loading and validation**

Create `incident-autopilot/internal/config/config.go` with typed sections for
SigNoz, target, signals, policy, pod outliers, and controller. `Validate` must
reject empty identifiers, unsupported modes, non-positive intervals, replica
bounds where `min > max`, and an SLI objective outside `(0, 1]`.

The public implementation must follow this shape:

```go
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SigNoz struct {
		URL       string `yaml:"url"`
		APIKeyEnv string `yaml:"api_key_env"`
	} `yaml:"signoz"`
	Target struct {
		Service       string `yaml:"service"`
		Environment   string `yaml:"environment"`
		Namespace     string `yaml:"namespace"`
		Deployment    string `yaml:"deployment"`
		ReadinessGate string `yaml:"readiness_gate"`
	} `yaml:"target"`
	Signals struct {
		RequestRateQuery string        `yaml:"request_rate_query"`
		P95LatencyQuery  string        `yaml:"p95_latency_query"`
		ErrorRateQuery   string        `yaml:"error_rate_query"`
		SLIQuery         string        `yaml:"sli_query"`
		SLIObjective     float64       `yaml:"sli_objective"`
		FreshnessLimit   time.Duration `yaml:"freshness_limit"`
	} `yaml:"signals"`
	Policy struct {
		MinReplicas              int32         `yaml:"min_replicas"`
		MaxReplicas              int32         `yaml:"max_replicas"`
		TargetRequestsPerReplica float64       `yaml:"target_requests_per_replica"`
		LatencyTargetMS          float64       `yaml:"latency_target_ms"`
		ErrorRateTarget          float64       `yaml:"error_rate_target"`
		MaxScaleUpStep           int32         `yaml:"max_scale_up_step"`
		MaxScaleDownStep         int32         `yaml:"max_scale_down_step"`
		Cooldown                 time.Duration `yaml:"cooldown"`
		PodOutlier               struct {
			MinimumRequests        int     `yaml:"minimum_requests"`
			ErrorRateMultiplier    float64 `yaml:"error_rate_multiplier"`
			LatencyMultiplier      float64 `yaml:"latency_multiplier"`
			ConsecutiveEvaluations int     `yaml:"consecutive_evaluations"`
		} `yaml:"pod_outlier"`
	} `yaml:"policy"`
	Controller struct {
		Mode               string        `yaml:"mode"`
		EvaluationInterval time.Duration `yaml:"evaluation_interval"`
		VerificationWindow time.Duration `yaml:"verification_window"`
		StatePath          string        `yaml:"state_path"`
	} `yaml:"controller"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SigNoz.URL == "" || c.Target.Service == "" ||
		c.Target.Namespace == "" || c.Target.Deployment == "" {
		return fmt.Errorf("signoz URL and target identifiers are required")
	}
	if c.Policy.MinReplicas < 1 || c.Policy.MaxReplicas < c.Policy.MinReplicas {
		return fmt.Errorf("invalid replica bounds")
	}
	switch c.Controller.Mode {
	case "dry-run", "approval", "automatic":
	default:
		return fmt.Errorf("unsupported controller mode %q", c.Controller.Mode)
	}
	if c.Signals.SLIObjective <= 0 || c.Signals.SLIObjective > 1 {
		return fmt.Errorf("SLI objective must be in (0,1]")
	}
	return nil
}
```

- [x] **Step 6: Add the runnable example configuration**

Create `incident-autopilot/config.example.yaml`:

```yaml
signoz:
  url: http://signoz:8080
  api_key_env: SIGNOZ_API_KEY
target:
  service: checkout-api
  environment: demo
  namespace: autopilot-demo
  deployment: checkout-api
  readiness_gate: autopilot.signoz.io/healthy
signals:
  request_rate_query: sum(rate(checkout_requests_total{service_name="checkout-api"}[2m]))
  p95_latency_query: histogram_quantile(0.95, sum by (le) (rate(checkout_duration_milliseconds_bucket{service_name="checkout-api"}[2m])))
  error_rate_query: sum(rate(checkout_requests_total{service_name="checkout-api",status="failed"}[2m])) / sum(rate(checkout_requests_total{service_name="checkout-api"}[2m]))
  sli_query: 1 - (sum(rate(checkout_requests_total{service_name="checkout-api",status="failed"}[5m])) / sum(rate(checkout_requests_total{service_name="checkout-api"}[5m])))
  sli_objective: 0.99
  freshness_limit: 60s
policy:
  min_replicas: 2
  max_replicas: 10
  target_requests_per_replica: 25
  latency_target_ms: 800
  error_rate_target: 0.02
  max_scale_up_step: 4
  max_scale_down_step: 1
  cooldown: 2m
  pod_outlier:
    minimum_requests: 20
    error_rate_multiplier: 3
    latency_multiplier: 2
    consecutive_evaluations: 3
controller:
  mode: approval
  evaluation_interval: 15s
  verification_window: 2m
  state_path: /var/lib/autopilot/state.json
```

Also create a minimal `incident-autopilot/Makefile` with `build` and `test`
targets.

- [x] **Step 7: Run tests and static checks**

Run:

```bash
go test ./internal/config -v
go vet ./...
```

Expected: PASS.

- [x] **Step 8: Commit the foundation**

```bash
git add incident-autopilot
git commit -m "feat: scaffold incident autopilot controller"
```

---

### Task 2: Containerized demo workload with deterministic failure modes

**Files:**
- Create: `demo-app/Dockerfile`
- Modify: `demo-app/src/app.js`
- Modify: `demo-app/src/instrumentation.js`
- Create: `incident-autopilot/deploy/demo-app.yaml`
- Create: `incident-autopilot/deploy/load-generator.yaml`

**Interfaces:**
- Produces: `/api/health/live`, `/api/health/ready`, and `/api/demo/behavior`
- Produces: resource attributes `k8s.namespace.name`, `k8s.deployment.name`, `k8s.pod.name`, and `k8s.pod.uid`
- Produces: deterministic `capacity` and `bad-pod` scenarios

- [x] **Step 1: Add deterministic health and behavior state**

Add a pod-local behavior object in `demo-app/src/app.js`:

```js
const podBehavior = {
  ready: true,
  inventoryDelayMs: Number(process.env.INVENTORY_DELAY_MS || 0),
  inventoryErrorRate: Number(process.env.INVENTORY_ERROR_RATE || 0),
};

const checkoutRequestsCounter = meter.createCounter('checkout_requests_total', {
  description: 'Checkout requests classified by outcome',
});

const checkoutDurationMilliseconds = meter.createHistogram('checkout_duration_milliseconds', {
  description: 'Checkout duration in milliseconds',
  unit: 'ms',
});

app.get('/api/health/live', (_req, res) => {
  res.status(200).json({ status: 'alive' });
});

app.get('/api/health/ready', (_req, res) => {
  res.status(podBehavior.ready ? 200 : 503).json({
    status: podBehavior.ready ? 'ready' : 'not_ready',
    pod: process.env.K8S_POD_NAME || 'local',
  });
});

app.post('/api/demo/behavior', (req, res) => {
  const { ready, inventoryDelayMs, inventoryErrorRate } = req.body;
  if (typeof ready === 'boolean') podBehavior.ready = ready;
  if (Number.isFinite(inventoryDelayMs)) podBehavior.inventoryDelayMs = inventoryDelayMs;
  if (Number.isFinite(inventoryErrorRate)) podBehavior.inventoryErrorRate = inventoryErrorRate;
  res.json(podBehavior);
});
```

In the `reserveInventory` span, apply `podBehavior.inventoryDelayMs`, use
`inventoryErrorRate` instead of random global chaos for the deterministic path,
and attach `inventory.queue_time_ms`. At the existing success and failure
completion points, record `checkoutRequestsCounter.add(1, {status})` and
`checkoutDurationMilliseconds.record(duration, {status})` using `success` or
`failed`. These underscore metric names are the exact names used by
`config.example.yaml`.

- [x] **Step 2: Add Kubernetes resource attributes**

Extend the resource in `demo-app/src/instrumentation.js`:

```js
const resource = new Resource({
  'service.name': process.env.OTEL_SERVICE_NAME || 'telemetry-shop-api',
  'service.version': '1.0.0',
  'deployment.environment': process.env.OTEL_DEPLOYMENT_ENVIRONMENT || 'local',
  'k8s.namespace.name': process.env.K8S_NAMESPACE_NAME || 'local',
  'k8s.deployment.name': process.env.K8S_DEPLOYMENT_NAME || 'local',
  'k8s.pod.name': process.env.K8S_POD_NAME || 'local',
  'k8s.pod.uid': process.env.K8S_POD_UID || 'local',
});
```

- [x] **Step 3: Create the image**

Create `demo-app/Dockerfile`:

```dockerfile
FROM node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci --omit=dev
COPY src ./src
COPY public ./public
EXPOSE 3000
CMD ["npm", "start"]
```

- [x] **Step 4: Create Kubernetes manifests**

Create `incident-autopilot/deploy/demo-app.yaml` with:

- Namespace `autopilot-demo`
- Deployment `checkout-api` with two replicas
- Startup, readiness, and liveness HTTP probes
- Downward API environment variables for pod name and UID
- OTLP endpoint pointing to the in-cluster collector
- Service exposing port 3000

Use this probe and Downward API section:

```yaml
spec:
  containers:
    - name: checkout-api
      image: telemetry-shop:dev
      readinessProbe:
        httpGet:
          path: /api/health/ready
          port: 3000
        periodSeconds: 5
      livenessProbe:
        httpGet:
          path: /api/health/live
          port: 3000
        periodSeconds: 10
      env:
        - name: K8S_POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: K8S_POD_UID
          valueFrom:
            fieldRef:
              fieldPath: metadata.uid
```

- [x] **Step 5: Create deterministic load**

Create `incident-autopilot/deploy/load-generator.yaml` using
`grafana/k6:0.57.0`. Mount a script that sends valid checkout requests and
supports `K6_VUS` and `K6_DURATION`.

- [ ] **Step 6: Build and smoke-test in Kind**

Run:

```bash
docker build -t telemetry-shop:dev demo-app
kind load docker-image telemetry-shop:dev --name autopilot
kubectl apply -f incident-autopilot/deploy/demo-app.yaml
kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=120s
```

Expected: two ready pods and `/api/health/ready` returns HTTP 200.

- [x] **Step 7: Commit the deterministic demo**

```bash
git add demo-app incident-autopilot/deploy
git commit -m "feat: add deterministic Kubernetes demo workload"
```

---

### Task 3: SigNoz query client and telemetry trust gate

**Files:**
- Create: `incident-autopilot/internal/signoz/client.go`
- Create: `incident-autopilot/internal/signoz/client_test.go`

**Interfaces:**
- Produces: `signoz.Client.QueryScalar(ctx, query, start, end) (signoz.Scalar, error)`
- Produces: `signoz.Client.Snapshot(ctx, cfg, replicas) (model.Snapshot, error)`
- `signoz.Scalar` distinguishes missing data from numeric zero

- [x] **Step 1: Write response-decoding tests**

Test these cases with `httptest.Server`:

```go
func TestQueryScalarRejectsMissingSeries(t *testing.T)
func TestQueryScalarRejectsPartialPoint(t *testing.T)
func TestQueryScalarUsesNewestCompletePoint(t *testing.T)
func TestSnapshotRejectsStaleSignals(t *testing.T)
```

The stale test must assert `errors.Is(err, signoz.ErrStaleTelemetry)`.

- [x] **Step 2: Run tests to verify they fail**

Run:

```bash
go test ./internal/signoz -v
```

Expected: FAIL because the package does not exist.

- [x] **Step 3: Implement the client**

Create these public contracts in `incident-autopilot/internal/signoz/client.go`:

```go
type Scalar struct {
	Value      float64
	ObservedAt time.Time
}

var (
	ErrNoData         = errors.New("no telemetry data")
	ErrStaleTelemetry = errors.New("telemetry is stale")
)

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(baseURL, apiKey string) *Client
func (c *Client) QueryScalar(
	ctx context.Context,
	query string,
	start time.Time,
	end time.Time,
) (Scalar, error)
```

Use `POST /api/v5/query_range`, set `SIGNOZ-API-KEY`, require a 2xx response,
reject partial points, and return the newest complete point. `Snapshot` must
query all service signals concurrently and fail closed if any required signal
is absent or older than `FreshnessLimit`.

- [x] **Step 4: Add pod-scoped query expansion**

Implement:

```go
func ExpandPodQuery(baseQuery, podName string) (string, error)
```

It must use a validated pod name and inject `k8s_pod_name="<pod>"` only into a
known template marker `${POD_FILTER}`. Do not concatenate untrusted text into
arbitrary PromQL.

- [x] **Step 5: Run tests**

```bash
go test ./internal/signoz -v
go test ./...
```

Expected: PASS.

- [x] **Step 6: Commit the SigNoz reader**

```bash
git add incident-autopilot/internal/signoz
git commit -m "feat: query trusted service telemetry from SigNoz"
```

---

### Task 4: Explainable incident classification and scaling policy

**Files:**
- Create: `incident-autopilot/internal/policy/engine.go`
- Create: `incident-autopilot/internal/policy/engine_test.go`

**Interfaces:**
- Consumes: `model.Snapshot` and `config.Policy`
- Produces: `policy.Engine.Evaluate(snapshot, now) model.Recommendation`

- [x] **Step 1: Write table-driven policy tests**

Cover:

```go
func TestCapacityPressureRecommendsBoundedScaleUp(t *testing.T)
func TestLowTrafficErrorsRecommendInvestigation(t *testing.T)
func TestOnePodOutlierRecommendsQuarantine(t *testing.T)
func TestMissingAvailabilityReturnsIndeterminate(t *testing.T)
func TestCooldownReturnsHold(t *testing.T)
func TestScaleDownIsConservative(t *testing.T)
```

The capacity test input must use:

```go
snapshot := model.Snapshot{
	CurrentReplicas: 2,
	Available:       2,
	RequestRate:     140,
	P95MS:           1200,
	ErrorRate:       0.08,
	SLI:             0.92,
}
```

With target 25 requests/replica and maximum step 4, expect six replicas.

- [x] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/policy -v
```

Expected: FAIL because `Engine` does not exist.

- [x] **Step 3: Implement deterministic classification**

Create `incident-autopilot/internal/policy/engine.go` with:

```go
type Engine struct {
	cfg          config.Config
	lastActionAt time.Time
	outlierRuns  map[string]int
}

func New(cfg config.Config) *Engine
func (e *Engine) Evaluate(s model.Snapshot, now time.Time) model.Recommendation
```

Decision order:

1. Reject unavailable or stale snapshots before calling the policy.
2. Hold during rollout or cooldown.
3. Detect a repeated pod outlier with sufficient traffic.
4. Classify low-traffic widespread errors as functional failure.
5. Compute `ceil(requestRate / targetRequestsPerReplica)`.
6. Add at most one latency pressure step.
7. Clamp to min/max and max per-action change.
8. Scale down by at most `MaxScaleDownStep` only when SLI, latency, and errors
   are all healthy for three consecutive evaluations.

- [x] **Step 4: Generate evidence and explanation without an LLM**

Build the recommendation reason from structured evidence. The capacity example
must render:

```text
Capacity pressure: request rate 140.0/s requires 6 replicas at 25.0/s per replica; P95 latency 1200ms exceeds 800ms; SLI 92.00% is below 99.00%.
```

- [x] **Step 5: Run tests**

```bash
go test ./internal/policy -v
```

Expected: PASS.

- [x] **Step 6: Commit the policy**

```bash
git add incident-autopilot/internal/policy
git commit -m "feat: add explainable autoscaling policy"
```

---

### Task 5: Controller loop, approval API, and decision telemetry

**Files:**
- Create: `incident-autopilot/internal/controller/controller.go`
- Create: `incident-autopilot/internal/controller/controller_test.go`
- Create: `incident-autopilot/internal/telemetry/emitter.go`
- Create: `incident-autopilot/internal/telemetry/http.go`
- Modify: `incident-autopilot/cmd/autopilot/main.go`

**Interfaces:**
- Produces: `controller.Controller.Evaluate(ctx) error`
- Produces: `GET /actions/latest` and `GET /actions/{id}` approval page
- Produces: `POST /api/actions/{id}/approve`
- Produces: `/metrics` for KEDA
- Produces: OTLP metrics and structured decision logs

- [x] **Step 1: Write controller-state tests**

Cover:

```go
func TestDryRunNeverPublishesScaleChange(t *testing.T)
func TestApprovalModeHoldsUntilApproved(t *testing.T)
func TestExpiredApprovalCannotExecute(t *testing.T)
func TestApprovalIsIdempotent(t *testing.T)
func TestIndeterminateSnapshotPublishesCurrentReplicas(t *testing.T)
```

- [x] **Step 2: Implement persisted state**

Persist this JSON atomically to `StatePath`:

```go
type State struct {
	LastRecommendation model.Recommendation `json:"last_recommendation"`
	LastAction         model.Action         `json:"last_action"`
	PublishedReplicas  int32                `json:"published_replicas"`
}
```

Write to a temporary file, `fsync`, then rename. Load it at startup. Never
execute an action twice for the same recommendation ID.

- [x] **Step 3: Implement approval authentication**

Create an HMAC token from recommendation ID and expiry:

```go
func SignApproval(secret []byte, id string, expiresAt time.Time) string
func VerifyApproval(secret []byte, token, id string, now time.Time) error
```

The approval endpoint must additionally record the authenticated operator name
from `X-Autopilot-Operator`. For the demo, protect the endpoint with a static
bearer secret stored in a Kubernetes Secret.

`GET /actions/latest` redirects to the current pending action.
`GET /actions/{id}` renders a minimal server-side HTML page containing the
immutable recommendation, evidence, expiry, and an approval form. The form posts
the signed recommendation ID to `POST /api/actions/{id}/approve`; it never
accepts a replica count or pod name from the browser.

- [x] **Step 4: Implement telemetry**

Export:

- `autopilot_recommended_replicas`
- `autopilot_current_replicas`
- `autopilot_pending_approval`
- `autopilot_decision_total`
- `autopilot_telemetry_freshness_seconds`
- `autopilot_heartbeat`

Expose only bounded labels: service, namespace, deployment, decision, reason
code, and policy version. Put action IDs and full explanations in structured
logs, not metric labels.

- [x] **Step 5: Run controller tests**

```bash
go test ./internal/controller ./internal/telemetry -v
```

Expected: PASS.

- [x] **Step 6: Commit the controller loop**

```bash
git add incident-autopilot/cmd incident-autopilot/internal/controller incident-autopilot/internal/telemetry
git commit -m "feat: add approval-gated controller loop"
```

---

### Task 6: KEDA scale-up integration

**Files:**
- Create: `incident-autopilot/deploy/autopilot.yaml`
- Create: `incident-autopilot/deploy/rbac.yaml`
- Create: `incident-autopilot/deploy/scaledobject.yaml`
- Create: `incident-autopilot/Dockerfile`

**Interfaces:**
- Consumes: `autopilot_recommended_replicas` from Autopilot `/metrics`
- Produces: KEDA `ScaledObject` and HPA controlling `checkout-api`

- [x] **Step 1: Package Autopilot**

Create a multi-stage `incident-autopilot/Dockerfile`:

```dockerfile
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/autopilot ./cmd/autopilot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/autopilot /autopilot
USER nonroot:nonroot
ENTRYPOINT ["/autopilot"]
```

- [x] **Step 2: Create least-privilege RBAC**

Grant:

- `get`, `list`, `watch` on Deployments, Pods, ReplicaSets, and EndpointSlices.
- `patch` on `pods/status` for the configured readiness gate.
- `delete` on Pods only for approved replacement.

Do not grant wildcard verbs or cluster-admin.

- [x] **Step 3: Create the KEDA ScaledObject**

Create `incident-autopilot/deploy/scaledobject.yaml`:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: checkout-api-autopilot
  namespace: autopilot-demo
spec:
  scaleTargetRef:
    name: checkout-api
  minReplicaCount: 2
  maxReplicaCount: 10
  pollingInterval: 15
  cooldownPeriod: 120
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 0
          policies:
            - type: Pods
              value: 4
              periodSeconds: 60
        scaleDown:
          stabilizationWindowSeconds: 180
          policies:
            - type: Pods
              value: 1
              periodSeconds: 60
  triggers:
    - type: prometheus
      metricType: Value
      metadata:
        serverAddress: http://incident-autopilot.autopilot-demo.svc:9090
        metricName: autopilot_recommended_replicas
        query: autopilot_recommended_replicas
        threshold: "1"
```

The integration contract for the MVP is exact: publishing
`autopilot_recommended_replicas 6` must result in an HPA desired replica count
of six. The integration test below is a release gate; do not continue to the
demo if it fails.

- [ ] **Step 4: Test dry-run and approval modes**

Run:

```bash
kubectl apply -f incident-autopilot/deploy/rbac.yaml
kubectl apply -f incident-autopilot/deploy/autopilot.yaml
kubectl apply -f incident-autopilot/deploy/scaledobject.yaml
kubectl -n autopilot-demo get hpa -w
```

Expected:

- Dry-run recommendation leaves replicas at two.
- Pending approval leaves replicas at two.
- Approved recommendation changes desired replicas to six.
- HPA never exceeds ten replicas.

- [x] **Step 5: Commit KEDA deployment**

```bash
git add incident-autopilot/deploy incident-autopilot/Dockerfile
git commit -m "feat: connect approved recommendations to KEDA"
```

---

### Task 7: Kubernetes rollout verification and incident reports

**Files:**
- Create: `incident-autopilot/internal/kube/client.go`
- Create: `incident-autopilot/internal/kube/client_test.go`
- Modify: `incident-autopilot/internal/controller/controller.go`
- Modify: `incident-autopilot/internal/telemetry/emitter.go`

**Interfaces:**
- Produces: `kube.Client.Target(ctx) (kube.TargetState, error)`
- Produces: `kube.Client.WaitForRollout(ctx, generation, timeout) error`
- Produces: `controller.Verify(ctx, before, recommendation) model.Verification`

- [x] **Step 1: Write fake-client tests**

Use `k8s.io/client-go/kubernetes/fake` to cover:

```go
func TestWaitForRolloutSucceedsWhenDesiredPodsAreReady(t *testing.T)
func TestWaitForRolloutTimesOutOnUnavailablePods(t *testing.T)
func TestTargetRejectsUnexpectedDeploymentUID(t *testing.T)
func TestVerificationMarksRecoveredWhenSLIMeetsObjective(t *testing.T)
func TestVerificationMarksIneffectiveWhenSLIStillFails(t *testing.T)
```

- [x] **Step 2: Implement Kubernetes target reading**

`TargetState` must include Deployment UID, generation, desired replicas,
available replicas, and owned Pods. Validate Pod owner references through the
ReplicaSet to the configured Deployment before permitting any pod action.

- [x] **Step 3: Implement post-scale verification**

After KEDA changes desired replicas:

1. Wait for Deployment `observedGeneration`.
2. Wait for desired available replicas.
3. Wait the configured verification window.
4. Query SigNoz again.
5. Compare SLI, P95, and errors with the pre-action snapshot.

Recovered means SLI meets objective and neither latency nor error rate worsened.
Improved means SLI increased but remains below objective. Ineffective means SLI
did not improve.

- [x] **Step 4: Emit an incident report**

Emit a structured OTLP log with:

```json
{
  "event.name": "autopilot.incident_report",
  "service.name": "checkout-api",
  "autopilot.recommendation_id": "stable-id",
  "autopilot.action": "scale_up",
  "autopilot.result": "recovered",
  "autopilot.before.sli": 0.92,
  "autopilot.after.sli": 0.995,
  "autopilot.before.replicas": 2,
  "autopilot.after.replicas": 6
}
```

- [x] **Step 5: Run tests**

```bash
go test ./internal/kube ./internal/controller ./internal/telemetry -v
```

Expected: PASS.

- [ ] **Step 6: Commit verification**

```bash
git add incident-autopilot/internal
git commit -m "feat: verify scaling recovery and report incidents"
```

---

### Task 8: Localized bad-pod quarantine and replacement

**Files:**
- Modify: `incident-autopilot/internal/kube/client.go`
- Modify: `incident-autopilot/internal/kube/client_test.go`
- Modify: `incident-autopilot/internal/controller/controller.go`
- Modify: `incident-autopilot/internal/policy/engine.go`

**Interfaces:**
- Produces: `kube.Client.SetAutopilotReady(ctx, pod, ready, reason) error`
- Produces: `kube.Client.WaitUntilNotRouted(ctx, podUID, timeout) error`
- Produces: `kube.Client.DeleteOwnedPod(ctx, podName, podUID) error`

- [ ] **Step 1: Write quarantine safety tests**

Cover:

```go
func TestQuarantineRejectsPodNotOwnedByTarget(t *testing.T)
func TestQuarantineSetsCustomReadinessConditionFalse(t *testing.T)
func TestDeleteWaitsUntilEndpointSliceNoLongerContainsPod(t *testing.T)
func TestReplacementFailurePreservesQuarantinedPod(t *testing.T)
func TestExpiredPodApprovalDoesNothing(t *testing.T)
```

- [ ] **Step 2: Patch only the custom readiness condition**

Modify `incident-autopilot/deploy/demo-app.yaml` to add:

```yaml
spec:
  template:
    spec:
      readinessGates:
        - conditionType: autopilot.signoz.io/healthy
```

At controller startup, watch target pods. When `ContainersReady=True` and the
pod has no active outlier finding, initialize the custom condition to
`ConditionTrue`. This prevents the opt-in readiness gate from leaving every new
pod permanently unready.

Use the Pod status subresource and preserve all existing conditions. Set:

```go
corev1.PodCondition{
	Type:               corev1.PodConditionType("autopilot.signoz.io/healthy"),
	Status:             corev1.ConditionFalse,
	Reason:             "TelemetryOutlier",
	Message:            "Quarantined after approved SigNoz Incident Autopilot recommendation",
	LastTransitionTime: metav1.Now(),
}
```

- [ ] **Step 3: Verify traffic drain**

Watch EndpointSlices selected by the Service. Proceed only when no endpoint with
the target pod UID is ready or serving. Timeout must abort without deleting the
pod.

- [ ] **Step 4: Add replacement capacity and delete safely**

Publish `currentReplicas + 1` through the approved KEDA recommendation, wait for
a new owned Pod to become ready, then delete the quarantined Pod using a UID
precondition:

```go
opts := metav1.DeleteOptions{
	Preconditions: &metav1.Preconditions{UID: &podUID},
}
```

After verification and cooldown, return to the normal policy recommendation.

- [ ] **Step 5: Run tests**

```bash
go test ./internal/kube ./internal/controller ./internal/policy -v
```

Expected: PASS.

- [ ] **Step 6: Commit pod remediation**

```bash
git add incident-autopilot/internal
git commit -m "feat: add approved unhealthy pod replacement"
```

---

### Task 9: Idempotent SigNoz dashboard and alert installer

**Files:**
- Create: `incident-autopilot/internal/installer/signoz.go`
- Create: `incident-autopilot/internal/installer/signoz_test.go`
- Modify: `incident-autopilot/cmd/autopilot/main.go`

**Interfaces:**
- Produces: `installer.EnsureDashboard(ctx) (string, error)`
- Produces: `installer.EnsureAlerts(ctx, channel string) error`
- Produces: CLI command `autopilot install`

- [ ] **Step 1: Write installer idempotency tests**

Use `httptest.Server` and assert:

- Existing dashboard is updated by stable title.
- Missing dashboard is created once.
- Existing alert is updated when its threshold changes.
- Notification channel must already exist; the installer never creates a fake
  no-op channel.

- [ ] **Step 2: Build the dashboard**

Generate panels for:

- Current versus recommended replicas.
- Request rate, P95 latency, error rate, and SLI.
- Pending approvals.
- Telemetry freshness.
- Decision count by reason.
- Scaling and quarantine actions.
- Pre/post verification.
- Per-pod outlier table.

Use exact metric names emitted in Task 5 and stable widget IDs.

- [ ] **Step 3: Build human-facing alerts**

Generate threshold alerts for:

- `autopilot_pending_approval > 0`
- `autopilot_heartbeat` absent for two minutes
- stale telemetry
- rollout or remediation failure
- maximum replicas reached while SLI is below objective

Alert annotations must link to the approval UI and the generated dashboard.

- [ ] **Step 4: Run tests**

```bash
go test ./internal/installer -v
```

Expected: PASS.

- [ ] **Step 5: Run against stock SigNoz twice**

```bash
./bin/autopilot install --config config.yaml --channel hackathon-email
./bin/autopilot install --config config.yaml --channel hackathon-email
```

Expected: exactly one dashboard and one copy of each alert after both runs.

- [ ] **Step 6: Commit SigNoz installation**

```bash
git add incident-autopilot/internal/installer incident-autopilot/cmd
git commit -m "feat: install autopilot dashboard and alerts"
```

---

### Task 10: Reproducible end-to-end demo and final verification

**Files:**
- Create: `incident-autopilot/DEMO.md`
- Create: `incident-autopilot/deploy/kind.yaml`
- Create: `incident-autopilot/scripts/demo-capacity.sh`
- Create: `incident-autopilot/scripts/demo-bad-pod.sh`
- Modify: `incident-autopilot/Makefile`
- Modify: `casting.yaml`
- Modify: `.gitignore`

**Interfaces:**
- Produces: one-command environment setup
- Produces: deterministic capacity and bad-pod demo scenarios

- [ ] **Step 1: Add developer commands**

The Makefile must provide:

```makefile
test:
	go test ./...

cluster:
	kind create cluster --name autopilot --config deploy/kind.yaml

deploy:
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/demo-app.yaml
	kubectl apply -f deploy/autopilot.yaml
	kubectl apply -f deploy/scaledobject.yaml

load:
	kubectl apply -f deploy/load-generator.yaml

demo-capacity:
	./scripts/demo-capacity.sh

demo-bad-pod:
	./scripts/demo-bad-pod.sh
```

- [ ] **Step 2: Write the capacity demo script**

The script must:

1. Confirm two ready replicas.
2. Start deterministic traffic.
3. Wait for an approval recommendation.
4. Print the recommendation and approval URL.
5. Pause for approval.
6. Wait for six ready replicas.
7. Query and print the verification result.

Every wait has a timeout and prints diagnostic commands on failure.

- [ ] **Step 3: Write the bad-pod demo script**

The script must:

1. Select one owned ready pod.
2. Set its deterministic inventory error rate to 100%.
3. Wait for three outlier evaluations.
4. Print the quarantine recommendation.
5. Pause for approval.
6. Confirm the pod leaves EndpointSlices before deletion.
7. Confirm the replacement becomes ready.
8. Print the recovered SLI.

- [ ] **Step 4: Write `DEMO.md`**

Document:

- Prerequisites and exact versions.
- SigNoz bootstrap and API-key creation.
- Notification-channel setup.
- Kind and KEDA installation.
- Build and deploy commands.
- Capacity-pressure script.
- Bad-pod script.
- Expected SigNoz dashboard states.
- Recovery commands.
- Troubleshooting for OTLP, KEDA, HPA, readiness gates, and EndpointSlices.

- [ ] **Step 5: Ignore local visual and runtime state**

Append:

```gitignore
.superpowers/
incident-autopilot/.state/
```

to `.gitignore`.

- [ ] **Step 6: Run the complete verification suite**

Run:

```bash
cd incident-autopilot
go test ./...
go vet ./...
docker build -t incident-autopilot:dev .
make deploy
make demo-capacity
make demo-bad-pod
```

Expected:

- All tests pass.
- Capacity scenario scales two to six after approval.
- SLI recovers and an incident report appears in SigNoz.
- Bad-pod scenario drains and replaces only the approved pod.
- No action executes when telemetry is deliberately stopped.

- [ ] **Step 7: Record the fallback demo**

Record a five-minute video showing:

1. Healthy baseline.
2. Capacity incident and explanation.
3. Notification and approval.
4. KEDA/HPA scale-up.
5. SLI recovery and incident report.
6. Localized bad-pod quarantine and replacement.

- [ ] **Step 8: Commit demo hardening**

```bash
git add .gitignore casting.yaml incident-autopilot
git commit -m "docs: add reproducible incident autopilot demo"
```

## Final hackathon acceptance checklist

- [ ] Stock SigNoz is used without source modifications.
- [ ] A service-account API key is sufficient for SigNoz integration.
- [ ] Every required query is scoped to service, environment, and pod where applicable.
- [ ] Missing or stale telemetry produces `indeterminate` and no action.
- [ ] The recommendation explains the telemetry evidence.
- [ ] A SigNoz notification asks for approval.
- [ ] Approval is scoped, expiring, idempotent, and audited.
- [ ] KEDA scales `checkout-api` from two to six replicas.
- [ ] Autopilot verifies SLI, latency, and error recovery.
- [ ] An incident report is queryable in SigNoz.
- [ ] One unhealthy pod can be drained and replaced only after approval.
- [ ] EndpointSlice removal is confirmed before pod deletion.
- [ ] RBAC is namespace- and workload-scoped.
- [ ] The demo runs from a fresh environment using documented commands.
