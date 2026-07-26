# Plan: standalone control-plane UI (replace shell-script demo flow)

## Problem with current setup

Today "testing" means: SSH-style juggling of `kubectl exec`, `load.sh`,
manually watching `kubectl get pods`, polling `/actions/latest` in a browser,
and hoping KEDA/the controller haven't drifted into a stale state (as just
happened — a stale `autopilot_recommended_replicas` fought every manual
`kubectl scale`). The React app is also embedded (`go:embed ui/dist/*`)
inside the Go binary and only reachable by port-forwarding straight into the
cluster (`18080`), so there's no persistent, controllable surface to run
from a laptop.

Goal: a standalone React app on `localhost` (its own dev server, not
embedded) talking to the existing Go controller over a proper HTTP API, that
can:
1. Show live system status (replicas, decision, SLI/P95/error rate, telemetry freshness) without touching kubectl.
2. Trigger/stop load tests (capacity pressure and bad-pod error injection) with parameters (VUs, delay, error rate) from the UI instead of shell scripts.
3. Show the current pending recommendation and let an operator approve/reject it in the same UI.
4. Show a history list of past recommendations/actions (approved, rejected, auto-expired) — not just "the latest one."

## What already exists (reuse, don't rebuild)

- `internal/controller/controller.go` — the `Controller` struct, `Evaluate()` loop, `Approve()`, `PendingRecommendation()`, state persistence (`State` struct → `state.json`). This stays as the decision engine; we only add API surface around it.
- `internal/kube/client.go` — already has `Replicas()`, `Target()`, pod ops. Reusable for a new "run load job" action.
- `internal/policy/engine.go` — untouched, still the source of truth for decisions.
- `scripts/load.sh` logic (inventory delay POST + Job manifest) — port this into a Go-side "load test" trigger instead of a shell script SSHing via `kubectl exec`/`kubectl apply`.
- React app skeleton in `internal/controller/ui/` (Vite + Tailwind + React 19) — keep the stack, move it out of `go:embed`, give it real routes and a proper API client.

## Root cause of today's confusion (context for whoever picks this up)

`autopilot_recommended_replicas` is the metric KEDA polls directly. It is
only ever written by `RecordPublishedReplicas`, called from `Approve()` (approval
mode) or `executeLocked()` (automatic mode). It survives controller pod
restarts via `state.json` on a mounted volume. If a scale-up was approved
and never scaled back down (no approved scale-down, and demo scripts bypass
approval by calling `reset_demo_baseline`'s KEDA-pause trick), the cluster
and the "recommended" metric drift apart, and any plain `kubectl scale` will
be immediately overridden by KEDA. The new UI must make this state visible
(current vs. recommended vs. published, with drift highlighted) instead of
silently fighting it via shell scripts.

## Architecture change

```
┌─────────────────────┐        HTTP/JSON        ┌──────────────────────────┐
│  React app (Vite)    │ ───────────────────────▶ │  Go controller (existing)│
│  localhost:5173      │ ◀─────────────────────── │  :8080 in-cluster        │
│  (own dev server,    │      new REST API         │  (port-forwarded to      │
│   NOT embedded)       │                          │   localhost:8080 by user)│
└─────────────────────┘                           └──────────────────────────┘
                                                              │
                                                              ▼
                                                   K8s API (existing kube.Client)
                                                   SigNoz API (existing signoz.Client)
```

- Go binary keeps running in-cluster exactly as it does today (still the thing that talks to K8s + SigNoz + KEDA). We are not moving the controller out of the cluster — only decoupling the UI from `go:embed` and giving it enough API to do everything the shell scripts currently do.
- `internal/controller/ui/` becomes a fully standalone Vite app: `npm run dev` on `localhost:5173`, calling the controller's API at `http://localhost:8080` (still reached via `kubectl port-forward`, or later an Ingress/LoadBalancer — out of scope for now). Vite dev proxy config forwards `/api/*` to `:8080` to dodge CORS during development.
- Drop the `go:embed ui/dist/*` block, the `fileServer`/`/assets/` routes, and `/actions/{id}` HTML-serving route from `controller.go`. The Go server becomes API-only.

## New Go API surface (additions to `controller.go` + a new `internal/loadtest` package)

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/status` | One-shot snapshot: current/available/recommended replicas, last decision, last snapshot (SLI/P95/error rate/request rate), telemetry freshness, controller mode, drift flag (recommended != actual observed). Replaces "watch kubectl get pods". |
| GET | `/api/actions` | List — last N recommendations/actions with outcome (approved/rejected/expired/superseded), timestamps, operator. Backed by extending `State` to keep a bounded ring buffer instead of only `LastRecommendation`/`LastAction`. |
| GET | `/api/actions/{id}` | Existing, unchanged (single recommendation detail + token). |
| POST | `/api/actions/{id}/approve` | Existing, unchanged. |
| POST | `/api/actions/{id}/reject` | New — explicit reject so it shows in history as "rejected" instead of silently expiring. Needs a `Reject()` method on `Controller` mirroring `Approve()`'s guard logic minus execution. |
| POST | `/api/loadtest/capacity` | New — body `{delayMs, vus, durationSeconds}`. Runs what `load.sh` does today: set `inventoryDelayMs` on all target pods via in-cluster exec/HTTP (reuse `kube.Client` to list pods; call their `/api/demo/behavior` endpoint over the pod IP directly instead of shelling into `kubectl exec` + `node -e`), then create the load-generator Job via `client-go`'s batch/v1 API (replaces the `kubectl apply -f -` heredoc). |
| POST | `/api/loadtest/badpod` | New — body `{targetPod?, errorRate}`. Picks (or accepts) a target pod, sets `inventoryErrorRate` on it only. Mirrors `demo-bad-pod.sh`. |
| POST | `/api/loadtest/stop` | New — deletes the `load-generator` Job and resets injected behavior (`inventoryDelayMs`/`inventoryErrorRate` back to 0) on all pods. Replaces manual `kubectl delete job` / cleanup step. |
| GET | `/api/loadtest/status` | New — is a load job currently running, VUs/duration/elapsed, which pods have injected behavior right now. |
| WS or SSE | `/api/stream` | New (stretch) — push status/decision changes so the UI doesn't need to poll `/api/status` every N seconds. If out of scope for v1, UI polls `/api/status` and `/api/actions` every 3-5s instead. |

New package `internal/loadtest` (mirrors `internal/kube`'s style):
- `Runner` struct wrapping `kubernetes.Interface`, holds target namespace/deployment/service from `config.Config`.
- `SetBehavior(ctx, podNames []string, delayMs, errorRate int) error` — replaces the `pod_http_post` bash function; do it as a real HTTP POST to `http://<pod-ip>:3000/api/demo/behavior` from inside the controller pod (it already runs in-cluster, so pod IPs are reachable — no more `kubectl exec` + inline Node script).
- `StartLoad(ctx, vus, durationSeconds int) error` — creates the Job via `clientset.BatchV1().Jobs(ns).Create(...)`, translating the same Job spec `load.sh` builds today.
- `StopLoad(ctx) error` — deletes the Job, ignoring not-found.
- `Status(ctx) (LoadStatus, error)` — reads the Job's pod phase/start time.

## State/model changes

- Extend `controller.State`:
  ```go
  type State struct {
      LastRecommendation model.Recommendation
      LastSnapshot        model.Snapshot
      LastAction          model.Action
      LastVerification    model.Verification
      PublishedReplicas   int32
      History             []model.HistoryEntry // NEW, capped ring buffer (e.g. last 50)
  }
  ```
- New `model.HistoryEntry{ Recommendation, Action, Outcome string, RecordedAt time.Time }` — `Outcome` ∈ `approved | rejected | expired | superseded`. Appended whenever `Evaluate()` produces a new non-hold decision, and updated when `Approve()`/`Reject()` resolve it or a newer recommendation supersedes an unresolved one.
- This is the only structural change to the decision engine's persisted state; `Evaluate()`/`Approve()` logic itself is untouched, just add an append call.

## React app changes (`internal/controller/ui/`, moved out of the Go module's embed but same directory is fine — just stop referencing it from `go:embed`)

New pages/components (React Router, not currently a dependency — add `react-router-dom`):
1. **Dashboard** (`/`) — live status cards (current/recommended/available replicas with drift warning if they disagree for more than one poll cycle), SLI/P95/error rate/request rrate, telemetry freshness, controller mode badge.
2. **Load Test Control** (`/loadtest`) — two forms (Capacity: VUs/delay/duration; Bad Pod: error rate/target pod dropdown), Start/Stop buttons, live status of any running job.
3. **Approvals** (`/actions`, default landing for a pending action — keep `/actions/latest` redirect behavior server-side or client-side) — current pending recommendation with the existing approve form, PLUS a history table below it (from `GET /api/actions`) showing past entries with outcome badges.
4. Shared `api.js` client wrapping `fetch` against `VITE_API_BASE` (defaults to same-origin in prod once reverse-proxied, or `http://localhost:8080` in dev via `.env.local`).

Vite config addition (`vite.config.js`):
```js
server: {
  proxy: { '/api': 'http://localhost:8080' }
}
```

## Migration / rollout order (so nothing breaks mid-way)

1. Add the new Go API endpoints (`/api/status`, `/api/actions` list, `/api/loadtest/*`, `/reject`) alongside the existing ones — purely additive, no removal yet. Ship + test with `curl` before touching the frontend.
2. Add `internal/loadtest` package + wire `StartLoad`/`StopLoad`/`SetBehavior` into `main.go`'s controller construction (new `Option`, same pattern as `WithMetricsHandler`).
3. Extend `State`/`model.HistoryEntry`, wire history recording into `Evaluate()`/`Approve()`, add `Reject()`.
4. Build the new React pages against the running Go API (still port-forwarded) — verify Dashboard, Load Test, Approvals end-to-end manually.
5. Only once the standalone app fully replaces the shell-script flow: remove `go:embed ui/dist/*`, the `fileServer`, `/assets/`, and `/actions/{id}` HTML routes from `controller.go`, and delete `scripts/load.sh`, `scripts/demo-capacity.sh`, `scripts/demo-bad-pod.sh`, `scripts/demo-lib.sh` (superseded by UI buttons). Keep `physical-verify.sh`/`physical-quarantine-verify.sh` — those test K8s mechanics directly, not the demo flow.
6. Update `DEMO.md` / `Makefile` (`demo-ready`, `demo-load`, `demo-cleanup` targets) to reflect: `make demo-ready` still brings up infra, but the "run load + approve" step becomes "open the React app and click Start" instead of `make demo-load`.

## Explicitly out of scope for this pass

- Moving the Go controller itself outside the cluster, or exposing it outside `kubectl port-forward` (Ingress/public URL) — not requested, adds real security surface (approval secret, SigNoz key) that needs more careful handling than "make it standalone."
- Fixing the SigNoz dashboard rendering bug (separate, already-diagnosed issue — schema the backend accepts but this SigNoz frontend build won't render). Independent of this plan.
- Auth for the React app itself — it inherits whatever the approval secret already gates (bearer token on the approve/reject calls); no new login system.
- Websocket/SSE live push is listed as a stretch goal; default plan is polling, simplest to ship correctly first.

## Open questions to confirm before implementation starts

1. Should `/api/loadtest/*` require the same bearer approval secret as `/approve`, or be open on the port-forwarded API (same trust boundary as today's scripts, which assume local-only access)?
2. History ring buffer size (proposed 50) — fine, or do you want unbounded/persisted-to-SigNoz-logs instead of in `state.json`?
3. Keep `/actions/latest` redirect behavior, or should the React app's Approvals page just always show "current pending" via `/api/status` and drop the special latest-redirect route entirely?
