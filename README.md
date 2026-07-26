# SigNoz Incident Autopilot

**Turn SigNoz telemetry into safe, explainable, approval-gated Kubernetes scaling.**

Incident Autopilot is a companion service for a **stock, unmodified SigNoz** installation. It continuously reads trusted metrics from SigNoz, explains when a workload needs more capacity (or when a single pod is unhealthy), publishes a bounded desired-replica signal for **KEDA**, and verifies whether the action restored service health.

Built for the SigNoz hackathon. Does **not** modify SigNoz source code.

---

## What it does

| Capability | Description |
|------------|-------------|
| **Multi-signal decisions** | Combines request rate, P95 latency, error rate, and SLI against configurable targets |
| **Explained recommendations** | Every scale/quarantine decision includes evidence and a human-readable reason |
| **Approval-gated actions** | Operator approves or rejects via a React control-plane UI before KEDA scales |
| **KEDA integration** | Publishes `autopilot_recommended_replicas`; KEDA ScaledObject drives the Deployment |
| **Bad-pod quarantine** | Detects per-pod outliers, drains via custom readiness gate, replaces the pod after approval |
| **Post-action verification** | Waits for rollout + observation window, classifies recovered / improved / ineffective |
| **SigNoz install command** | Creates dashboard + alerts in your existing SigNoz instance |
| **Fail-closed on bad telemetry** | Refuses to act when SigNoz data is stale or incomplete |

---

## Architecture (demo)

```
┌─────────────────────┐     HTTP/JSON      ┌──────────────────────────┐
│  React UI           │ ─────────────────▶ │  Go controller (in K8s)  │
│  localhost:5173     │ ◀───────────────── │  incident-autopilot pod  │
└─────────────────────┘                    └───────────┬──────────────┘
                                                       │
                         ┌─────────────────────────────┼─────────────────────────────┐
                         ▼                             ▼                             ▼
                   SigNoz (Docker)              checkout-api pods              KEDA → HPA
                   metrics + alerts              (demo workload)                scales replicas
```

- **SigNoz** runs locally via Docker Compose (`make up`).
- **checkout-api** and **incident-autopilot** run in a local **Kind** cluster with **KEDA**.
- The **React UI** is a standalone Vite app; it talks to the controller API through a port-forward.

---

## Repository layout

| Path | Purpose |
|------|---------|
| `incident-autopilot/` | Go controller, policy engine, load-test API, React UI |
| `demo-app/` | Telemetry Shop — sample checkout workload with OTel instrumentation |
| `pours/deployment/` | Stock SigNoz Docker Compose stack |
| `incident-autopilot/DEMO.md` | Detailed demo walkthrough and troubleshooting |
| `incident-autopilot/PHASES.md` | Implementation checklist and manual test guide |

---

## Prerequisites

| Tool | Version |
|------|---------|
| Docker Desktop / Engine | 24+ |
| Kind | 0.26+ |
| kubectl | 1.30+ |
| Helm | 3.14+ |
| Go | 1.25+ |
| Node.js | 20 LTS |
| jq | any recent |

**Hardware:** 8 GB RAM minimum (16 GB recommended) for SigNoz + Kind + KEDA.

---

## Quick start (judges / first run)

### 1. Configure secrets

```bash
cd incident-autopilot
cp .env.example .env.local
```

Edit `.env.local`:

```bash
SIGNOZ_API_KEY=<your SigNoz API key>   # Settings → API Keys in SigNoz UI
AUTOPILOT_APPROVAL_SECRET=dev-approval-secret
```

Create a notification channel in SigNoz named **`hackathon-email`** (or change the channel name in the install command below).

### 2. Bootstrap everything

From the **repository root**:

```bash
make demo-ready
```

This idempotently: starts SigNoz, installs dashboard/alerts, creates the Kind cluster + KEDA, builds and deploys images, applies secrets, and resets a **2-replica baseline**.

### 3. Start the control-plane UI

**Terminal 1 — API tunnel**

```bash
kubectl -n autopilot-demo port-forward svc/incident-autopilot 8090:8080
```

**Terminal 2 — React UI**

```bash
cd incident-autopilot/internal/controller/ui
npm install
npm run dev
```

Open **http://localhost:5173**

Optional (so Approve works without fetching the secret from the API):

```bash
# incident-autopilot/internal/controller/ui/.env.local
VITE_APPROVAL_SECRET=dev-approval-secret
```

### 4. Service URLs

| Service | URL |
|---------|-----|
| Control-plane UI | http://localhost:5173 |
| Controller API (port-forward) | http://localhost:8090 |
| SigNoz UI | http://localhost:8080 |
| SigNoz OTLP (HTTP) | http://localhost:4318 |

SigNoz and the controller both listen on **8080 inside their respective environments**; on your laptop we use **8080 for SigNoz** and **8090 for the controller** to avoid a clash.

---

## Demo script (5–10 minutes)

### A. Capacity pressure → scale up

1. **Dashboard** (`/`) — confirm 2 replicas, decision `hold`, SLI ≈ 99%.
2. **Load Test** (`/loadtest`) — Capacity: delay **1500** ms, **40** VUs, **300** s → **Start**.
3. **Dashboard** — within 1–2 min, P95 and request rate rise; decision becomes **`scale_up`**.
4. **Actions** (`/actions`) — enter operator name → **Approve**.
5. **Dashboard** — recommended and current replicas climb (KEDA scales toward approved target).
6. **Load Test** → **Stop**.

### B. Bad pod → quarantine

1. Reset to 2 replicas if needed: `kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2`
2. **Load Test** → Bad Pod: error rate **100%**, target pod blank (auto-pick) → **Start**.
3. Wait for **`quarantine_replace`** on Dashboard / Actions.
4. **Approve** — target pod is drained and replaced.
5. Verify: `kubectl -n autopilot-demo get pods -l app=checkout-api`

### C. Reject path (optional)

Trigger capacity load again; when `scale_up` appears, click **Reject** on Actions. Dashboard should show **no scale change**; history logs `rejected`.

---

## Testing

### Unit tests (no cluster required)

```bash
# From repo root
make test

# Or directly
cd incident-autopilot
go test ./...
go vet ./...
```

Covers policy engine, controller state/approval/reject/history, load-test package (fake K8s client), SigNoz client, kube client, and telemetry.

### End-to-end setup script

```bash
cd incident-autopilot

./scripts/test-e2e.sh unit      # unit tests only
./scripts/test-e2e.sh setup     # same as make demo-ready (infra)
./scripts/test-e2e.sh capacity  # POST /api/loadtest/capacity (approve in UI)
./scripts/test-e2e.sh badpod    # POST /api/loadtest/badpod (approve in UI)
./scripts/test-e2e.sh all       # unit + setup
```

Requires `.env.local` with `SIGNOZ_API_KEY`. Load steps need port-forward on `:8090` (set `AUTOPILOT_API_URL=http://127.0.0.1:8090` if using the script).

### Physical / K8s verification

```bash
make physical-verify              # rollout + scaling mechanics
make physical-quarantine-verify   # readiness gate + drain + replace
```

### Manual API checks (port-forward on 8090)

```bash
curl -s http://localhost:8090/api/status | jq
curl -s http://localhost:8090/api/actions | jq
curl -s http://localhost:8090/metrics | grep autopilot_recommended_replicas

curl -X POST http://localhost:8090/api/loadtest/capacity \
  -H 'Content-Type: application/json' \
  -d '{"delayMs":1500,"vus":40,"durationSeconds":300}'

curl -X POST http://localhost:8090/api/loadtest/stop
```

### Build verification

```bash
cd incident-autopilot
go build -o bin/autopilot ./cmd/autopilot
docker build -t incident-autopilot:dev .
cd internal/controller/ui && npm run build
```

---

## Operating modes

Configure in `incident-autopilot/config.local.yaml`:

| Mode | Behavior |
|------|----------|
| `dry-run` | Evaluates and recommends; never publishes replica changes |
| `approval` | Publishes scale changes only after operator approval (**demo default**) |
| `automatic` | Publishes scale changes immediately (quarantine still approval-only) |

---

## Makefile reference

```bash
make help              # All targets
make up                # SigNoz + OTLP collection agent (Docker)
make down              # Stop Docker services
make demo-ready        # Full K8s demo baseline
make demo-load         # Port-forward hints + UI instructions
make demo-cleanup      # Stop load job, scale back to 2 replicas
make k8s-setup         # Kind + KEDA
make k8s-build-images  # Build and load images into Kind
make k8s-deploy        # Apply manifests
make clean             # Remove local build artifacts
```

---

## Cleanup

```bash
make demo-cleanup
kind delete cluster --name autopilot
make down
```

---

## Further reading

- [incident-autopilot/DEMO.md](incident-autopilot/DEMO.md) — step-by-step demo, troubleshooting, recovery
- [incident-autopilot/PHASES.md](incident-autopilot/PHASES.md) — API surface and manual test checklist
- [docs/superpowers/specs/2026-07-24-signoz-incident-autopilot-design.md](docs/superpowers/specs/2026-07-24-signoz-incident-autopilot-design.md) — design spec

---

## Hackathon highlights

- **Stock SigNoz only** — queries via API; installs dashboard/alerts without forking SigNoz
- **Explainable, bounded scaling** — policy engine with cooldowns, max step sizes, and evidence
- **Human in the loop** — React UI for status, load tests, approve/reject, and action history
- **Verifiable outcomes** — post-action SLI/latency/error comparison logged to SigNoz
- **Production-shaped K8s** — KEDA, custom readiness gates, EndpointSlice drain before pod delete
