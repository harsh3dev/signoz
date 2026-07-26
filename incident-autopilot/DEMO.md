# SigNoz Incident Autopilot — Reproducible Demo

This guide walks through the full hackathon demo using the **React control-plane UI**:
SigNoz telemetry drives an explained, approval-gated KEDA scale-up, SLI verification,
and localized bad-pod quarantine.

## Prerequisites (tested versions)

| Tool | Version |
|------|---------|
| Docker Desktop / Engine | 24+ |
| Kind | 0.26+ |
| kubectl | 1.30+ |
| Helm | 3.14+ |
| Go | 1.25+ |
| Node.js | 20 LTS |

Hardware: 8 GB RAM minimum (16 GB recommended) for SigNoz + Kind + KEDA.

## 1. Bootstrap SigNoz (stock, unmodified)

From the repository root:

```bash
make up
curl -sf http://localhost:8080/api/v1/health
```

Create a SigNoz API key (Settings → API Keys, read access) and export:

```bash
export SIGNOZ_API_KEY="<your-key>"
```

## 2. Notification channel

Create a notification channel in SigNoz named `hackathon-email` (or any name you prefer).

## 3. Install autopilot dashboard and alerts

```bash
cd incident-autopilot
cp .env.example .env.local   # optional
export SIGNOZ_API_KEY
export AUTOPILOT_APPROVAL_SECRET=dev-approval-secret

go build -o bin/autopilot ./cmd/autopilot
./bin/autopilot install \
  --config config.local.yaml \
  --channel hackathon-email \
  --approval-url http://localhost:5173
```

Re-running install must not create duplicate dashboards or alerts.

## 4. Kind cluster and KEDA

```bash
make k8s-setup    # from repo root
```

## 5. Build and deploy

```bash
cd incident-autopilot
make build        # docker image
# or: make build-local

# Load images into Kind (from repo root)
make k8s-build-images
make k8s-deploy

kubectl create secret generic incident-autopilot-approval \
  --namespace autopilot-demo \
  --from-literal=secret="${AUTOPILOT_APPROVAL_SECRET:-dev-approval-secret}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic signoz-credentials \
  --namespace autopilot-demo \
  --from-literal=api-key="${SIGNOZ_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s
kubectl -n autopilot-demo rollout status deployment/incident-autopilot --timeout=180s
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
```

Or from repo root: `make demo-ready`

## 6. Start the control-plane UI

Port-forward the controller API (Vite proxies `/api` to this):

```bash
kubectl -n autopilot-demo port-forward svc/incident-autopilot 8080:8080
```

In another terminal:

```bash
cd incident-autopilot/internal/controller/ui
npm install
npm run dev
```

Open **http://localhost:5173**

Optional: set `VITE_APPROVAL_SECRET=dev-approval-secret` in `internal/controller/ui/.env.local` so the Actions page can approve without fetching the secret from the API.

## 7. Capacity-pressure demo (UI)

1. **Dashboard** (`/`) — confirm 2/2 replicas, decision `hold`, SLI near 99%.
2. **Load Test** (`/loadtest`) — Capacity form: delay 1500 ms, 40 VUs, 300 s → **Start**.
3. **Dashboard** — within 1–2 min, P95 and request rate rise; decision becomes `scale_up`.
4. **Actions** (`/actions`) — pending recommendation appears; enter operator name → **Approve**.
5. **Dashboard** — recommended and current replicas climb toward the approved target.
6. **Load Test** → **Stop** when done.

## 8. Bad-pod demo (UI)

Run after capacity demo has settled (or scale checkout-api back to 2 replicas).

1. **Load Test** → Bad Pod: error rate 100%, leave target pod blank (auto-pick) → **Start**.
2. Wait ~1–2 min for `quarantine_replace` on **Dashboard** / **Actions**.
3. **Approve** on Actions page.
4. Confirm the target pod is replaced (`kubectl -n autopilot-demo get pods`).

## API reference (curl)

Same operations without the UI (port-forward on `:8080`):

```bash
curl -s http://localhost:8080/api/status | jq
curl -X POST http://localhost:8080/api/loadtest/capacity \
  -H 'Content-Type: application/json' \
  -d '{"delayMs":1500,"vus":40,"durationSeconds":300}'
curl -X POST http://localhost:8080/api/loadtest/stop
curl -s http://localhost:8080/api/actions | jq
curl -X POST http://localhost:8080/api/actions/<id>/reject \
  -H 'X-Autopilot-Operator: manual-test'
```

## Recovery commands

```bash
curl -X POST http://localhost:8080/api/loadtest/stop
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
kind delete cluster --name autopilot
cd .. && make down
```

## Troubleshooting

- **404 on /api/status** — controller not running or port-forward down.
- **Stale telemetry** — check SigNoz metrics and `signoz-credentials` secret.
- **KEDA not scaling** — `curl -s http://localhost:8080/metrics | grep autopilot_recommended_replicas`
- **Replica drift** — Dashboard shows current vs recommended mismatch; approve a proper scale-down instead of fighting with `kubectl scale`.

## Verification suite

```bash
cd incident-autopilot
go test ./...
go vet ./...
docker build -t incident-autopilot:dev .
```

Full manual loop: `make demo-ready` then start the UI (section 6) and run sections 7–8.
