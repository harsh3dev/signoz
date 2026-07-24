# SigNoz Incident Autopilot — Reproducible Demo

This guide walks through the full hackathon demo: SigNoz telemetry drives an
explained, approval-gated KEDA scale-up, SLI verification, and localized
bad-pod quarantine.

## Prerequisites (tested versions)

| Tool | Version |
|------|---------|
| Docker Desktop / Engine | 24+ |
| Kind | 0.26+ |
| kubectl | 1.30+ |
| Helm | 3.14+ |
| Go | 1.25+ |
| Node.js | 20 LTS (for local Telemetry Shop) |

Hardware: 8 GB RAM minimum (16 GB recommended) for SigNoz + Kind + KEDA.

## 1. Bootstrap SigNoz (stock, unmodified)

From the repository root:

```bash
make up
```

This starts:

- SigNoz UI at `http://localhost:8080`
- OTLP HTTP ingest at `http://localhost:4318`
- Collection agent on `http://localhost:14318` (for local demo app traffic)

Verify health:

```bash
curl -sf http://localhost:8080/api/v1/health
```

### Create a service-account API key

1. Open `http://localhost:8080` → **Settings** → **Account Settings** → **API Keys**.
2. Create a key with **read** access (queries + dashboard install).
3. Export it:

```bash
export SIGNOZ_API_KEY="<your-key>"
```

## 2. Notification channel

Create an email (or webhook) notification channel in SigNoz named
`hackathon-email` (or any name you prefer). Alerts installed by autopilot route
to this channel.

## 3. Install autopilot dashboard and alerts

```bash
cd incident-autopilot
cp .env.example .env.local   # optional for local runs
export SIGNOZ_API_KEY
export AUTOPILOT_APPROVAL_SECRET=dev-approval-secret

go build -o bin/autopilot ./cmd/autopilot
./bin/autopilot install \
  --config config.local.yaml \
  --channel hackathon-email \
  --approval-url http://localhost:8090
```

Re-running the install command must not create duplicate dashboards or alerts.

## 4. Kind cluster and KEDA

```bash
cd incident-autopilot
make cluster
../scripts/k8s-setup.sh   # installs KEDA via Helm
```

Or from the repo root:

```bash
make k8s-setup
```

## 5. Build and deploy

```bash
cd incident-autopilot

# Build images and load into Kind
docker build -t incident-autopilot:dev .
docker build -t telemetry-shop:dev ../demo-app
kind load docker-image incident-autopilot:dev --name autopilot
kind load docker-image telemetry-shop:dev --name autopilot

# Kubernetes secrets (one-time)
kubectl create secret generic incident-autopilot-approval \
  --namespace autopilot-demo \
  --from-literal=secret="${AUTOPILOT_APPROVAL_SECRET:-dev-approval-secret}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic signoz-credentials \
  --namespace autopilot-demo \
  --from-literal=api-key="${SIGNOZ_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

make deploy
kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s
kubectl -n autopilot-demo rollout status deployment/incident-autopilot --timeout=180s
```

## 6. Capacity-pressure demo

Generates deterministic load + per-pod inventory delay, waits for a `scale_up`
recommendation, pauses for approval, then verifies six replicas and SLI recovery.

```bash
cd incident-autopilot
make load          # optional; demo-capacity starts its own load job
make demo-capacity
```

Environment overrides:

| Variable | Default | Purpose |
|----------|---------|---------|
| `DEMO_AUTO_APPROVE` | `false` | Set `true` for unattended runs |
| `CAPACITY_TARGET_REPLICAS` | `6` | Expected scale target |
| `K6_VUS` | `40` | Load generator concurrency |
| `SIGNOZ_API_KEY` | unset | Enables recovered SLI query at end |

Approval URL (when port-forward is active): `http://127.0.0.1:18080/actions/latest`

## 7. Bad-pod demo

Injects 100% inventory errors on one pod, waits for three outlier evaluations,
pauses for quarantine approval, confirms EndpointSlice drain, and prints SLI.

```bash
cd incident-autopilot
make demo-bad-pod
```

Run **after** the capacity demo has returned to a stable two-replica baseline,
or scale checkout-api back to two replicas first:

```bash
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
```

## Expected SigNoz dashboard states

Open the **Incident Autopilot** dashboard (installed by `autopilot install`):

1. **Healthy baseline** — SLI ≥ 99%, P95 < 800 ms, two replicas.
2. **Capacity incident** — rising P95 and request rate; recommendation panel shows `scale_up` with evidence.
3. **Post-approval** — `autopilot_recommended_replicas` publishes 6; HPA desired count follows.
4. **Verification** — SLI recovers; structured `autopilot.incident_report` log appears in Logs Explorer.
5. **Bad pod** — one pod's error rate diverges; recommendation shows `quarantine_replace` with target pod.
6. **Recovery** — quarantined pod leaves endpoints; replacement becomes ready; SLI returns to objective.

## Recovery commands

```bash
# Stop load
kubectl -n autopilot-demo delete job load-generator --ignore-not-found

# Reset checkout-api scale
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2

# Remove bad-pod behavior (per pod)
kubectl -n autopilot-demo port-forward pod/<pod-name> 3000:3000
curl -X POST http://localhost:3000/api/demo/behavior \
  -H 'Content-Type: application/json' \
  -d '{"inventoryErrorRate":0,"inventoryDelayMs":0}'

# Tear down Kubernetes
kind delete cluster --name autopilot

# Stop SigNoz
cd .. && make down
```

## Troubleshooting

### OTLP / missing telemetry

- Symptom: recommendations stay `indeterminate`; no action executes.
- Check checkout pods export to `http://host.docker.internal:4318`.
- Confirm metrics exist in SigNoz Metrics Explorer: `checkout_requests_total`.
- Verify `signoz-credentials` secret and `SIGNOZ_API_KEY` in the autopilot pod.

### KEDA / HPA not scaling

```bash
kubectl -n autopilot-demo get scaledobject,hpa
kubectl -n keda logs deploy/keda-operator --tail=50
curl -s http://127.0.0.1:18080/metrics | grep autopilot_recommended_replicas
```

KEDA reads `autopilot_recommended_replicas` from the autopilot Prometheus API
at `http://incident-autopilot.autopilot-demo.svc:8080/api/v1/query`.

### Readiness gates

New pods include `autopilot.signoz.io/healthy`. The controller initializes this
condition to `True` for healthy pods. If pods stay unready:

```bash
kubectl -n autopilot-demo describe pod -l app=checkout-api
kubectl -n autopilot-demo logs deploy/incident-autopilot --tail=100
```

### EndpointSlices / drain verification

```bash
kubectl -n autopilot-demo get endpointslices -l kubernetes.io/service-name=checkout-api -o yaml
```

Quarantine sets the custom readiness gate to `False` and waits until the pod
UID disappears from ready endpoints before deletion.

## Complete verification suite

```bash
cd incident-autopilot
go test ./...
go vet ./...
docker build -t incident-autopilot:dev .
make deploy
make demo-capacity
make demo-bad-pod
```

For unattended CI-style runs:

```bash
export DEMO_AUTO_APPROVE=true
export SIGNOZ_API_KEY
make demo-capacity
make demo-bad-pod
```

## Fallback demo recording (5 minutes)

Record these scenes in order:

1. Healthy baseline dashboard.
2. Capacity incident with explained recommendation.
3. SigNoz notification and browser approval.
4. KEDA/HPA scale-up to six replicas.
5. SLI recovery and incident report in SigNoz Logs.
6. Localized bad-pod quarantine and replacement.
