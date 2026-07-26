# Phased implementation plan + manual test guide

Companion to `PLAN.md` (architecture/rationale). This file is the execution
checklist: do the phases in order, each phase ends in a working, testable
state — never leave the repo mid-phase broken.

---

## Phase 0 — Decisions (no code)

Before writing anything, resolve the 3 open questions from `PLAN.md`:

1. Do `/api/loadtest/*` endpoints require the bearer approval secret?
   **No** — same trust boundary as today's port-forwarded scripts (local-only).
2. History ring buffer size — 50 ok?
   **Yes** — cap at 50 entries in persisted `state.json`.
3. Keep `/actions/latest` redirect, or drop it for `/api/status`-driven UI?
   **Drop it** — React Approvals page reads pending state from `/api/status`.

**Exit criteria:** answers written down (as comments in this file or told to
the implementer) before Phase 1 starts.

---

## Phase 1 — Read-only status API (Go)

**Files touched:** `internal/controller/controller.go`, maybe a new
`internal/controller/status.go`.

**Steps:**
1. Add `GET /api/status` handler: return JSON with
   `currentReplicas`, `availableReplicas`, `recommendedReplicas`
   (=`PublishedReplicas`), `lastDecision`, `lastSnapshot` (SLI/P95/error
   rate/request rate/observedAt), `telemetryFreshnessSeconds`, `mode`
   (from `cfg.Controller.Mode`), and a computed `drift` bool
   (`currentReplicas != recommendedReplicas` sustained — just report the raw
   mismatch for now, no time-window logic yet).
2. Register the route in `Router()` next to the existing `/api/actions/{id}`.
3. No changes to `Evaluate()`/`Approve()` — purely additive read.

**Exit criteria:** `curl http://localhost:8080/api/status | jq` returns a
populated JSON object while the controller pod is running.

---

## Phase 2 — Actions history (Go)

**Files touched:** `internal/model/model.go` (new `HistoryEntry` type),
`internal/controller/controller.go` (`State.History`, append logic, new
`GET /api/actions` list handler, new `Reject()` method + `POST
/api/actions/{id}/reject` handler).

**Steps:**
1. Add `model.HistoryEntry{ Recommendation, Action, Outcome string,
   RecordedAt time.Time }`.
2. In `State`, add `History []model.HistoryEntry` (bounded, cap from Phase 0
   answer).
3. In `Evaluate()`, when a new non-hold decision is produced and differs
   from `LastRecommendation.ID`, append a `pending` history entry (or mark
   the previous unresolved entry `superseded` if one exists).
4. In `Approve()`, after a successful approve, update the matching history
   entry's `Outcome` to `approved`.
5. Add `Reject(ctx, id, operator string) error` mirroring `Approve()`'s
   lookup/guard logic, minus execution — just marks history `Outcome =
   rejected` and clears `LastRecommendation` pending state so it stops
   showing as actionable.
6. Add handlers: `GET /api/actions` (return `State.History`, newest first),
   `POST /api/actions/{id}/reject`.
7. Update `persistStateLocked`/`loadState` — no changes needed, they
   already serialize the whole `State` struct via JSON.

**Exit criteria:**
- `curl -X POST http://localhost:8080/api/actions/{pending-id}/reject -H "Authorization: Bearer $SECRET"` marks it rejected.
- `curl http://localhost:8080/api/actions | jq` shows it with `"outcome": "rejected"`.
- Restart the controller pod → history survives (persisted state).

---

## Phase 3 — `internal/loadtest` package (Go)

**Files touched:** new `internal/loadtest/loadtest.go`,
`internal/loadtest/loadtest_test.go`, `cmd/autopilot/main.go` (wire it in),
`internal/controller/controller.go` (new `Option` + handlers).

**Steps:**
1. `Runner` struct: holds `kubernetes.Interface`, namespace, deployment
   label selector, service name/port for demo-app behavior POSTs.
2. `SetBehavior(ctx, delayMs, errorRate int, targetPod string) error` —
   `targetPod == ""` means "all ready pods matching the deployment
   selector." List pods via the clientset, POST directly to
   `http://<pod-ip>:3000/api/demo/behavior` from inside the controller pod
   (real `net/http` client — no more `kubectl exec`).
3. `StartCapacityLoad(ctx, vus, durationSeconds int) error` — build the same
   Job spec `scripts/load.sh` inlines today (`batchv1.Job`), submit via
   `clientset.BatchV1().Jobs(ns).Create(...)`. Delete any prior
   `load-generator` Job first (ignore not-found), matching current script
   behavior.
4. `StopLoad(ctx) error` — delete the Job (ignore not-found), then call
   `SetBehavior(ctx, 0, 0, "")` to clear injected behavior on all pods.
5. `Status(ctx) (LoadStatus, error)` — read the Job (exists?/pod
   phase/`StartTime`), and which pods currently report non-zero injected
   behavior (either track this in-memory in `Runner` since the last
   `SetBehavior` call, or skip — the demo app doesn't expose a GET for
   current behavior, so in-memory tracking is simplest).
6. Wire into `main.go`: construct `loadtest.NewRunner(...)`, pass into
   `controller.New(...)` via a new `controller.WithLoadRunner(...)` Option.
7. Add handlers on `Controller`: `POST /api/loadtest/capacity` (body
   `{delayMs, vus, durationSeconds}`), `POST /api/loadtest/badpod` (body
   `{targetPod?, errorRate}` — if `targetPod` omitted, pick one via existing
   pod-listing logic, mirroring `pick_target_pod` from `demo-lib.sh`), `POST
   /api/loadtest/stop`, `GET /api/loadtest/status`.

**Exit criteria (do this against the real kind cluster, not unit tests
alone):**
- `curl -X POST http://localhost:8080/api/loadtest/capacity -d '{"delayMs":1500,"vus":40,"durationSeconds":300}'` → `kubectl -n autopilot-demo get job load-generator` shows it running, and `kubectl exec` into a checkout-api pod confirms delay was set (or just trust the SLI dropping in SigNoz).
- `curl -X POST http://localhost:8080/api/loadtest/stop` → Job disappears, next evaluation cycle shows behavior reset.
- Unit tests in `loadtest_test.go` cover `SetBehavior`/`StartCapacityLoad`/`StopLoad` against a fake clientset (`k8s.io/client-go/kubernetes/fake`), following the existing pattern in `internal/kube/client_test.go`.

---

## Phase 4 — Decouple React app from `go:embed`

**Files touched:** `internal/controller/controller.go` (remove embed +
static-file routes), `internal/controller/ui/vite.config.js` (add dev
proxy), `Dockerfile` (remove UI build stage — or keep it if you still want
an optional embedded fallback; plan assumes removal), `Makefile`/`incident-autopilot/Makefile` (`ui-build` target becomes irrelevant to the Go build).

**Steps:**
1. Remove `//go:embed ui/dist/*`, `var uiFS embed.FS`, the `fileServer`,
   `/assets/` route, and the `/actions/{id}` HTML-serving route from
   `Router()`. Keep `/actions/latest` only if Phase 0 decided to keep it —
   otherwise remove that too.
2. `internal/controller/ui/vite.config.js`: add
   ```js
   server: { proxy: { '/api': 'http://localhost:8080' } }
   ```
3. Confirm `go build ./cmd/autopilot` still compiles with the embed block
   gone (no other code references `uiFS`).
4. `Dockerfile`: drop the `FROM node:20-alpine AS ui` stage and the
   `COPY --from=ui /ui/dist ...` line — the built image no longer serves
   any frontend.

**Exit criteria:**
- `go build -o bin/autopilot ./cmd/autopilot` succeeds.
- `curl http://localhost:8080/actions/some-id` now 404s (no longer serves HTML) — expected, this is intentional since the UI moved out.
- `cd internal/controller/ui && npm run dev` starts a dev server (default Vite port 5173) with **no visible content yet** (App.jsx still has old single-page logic — that's fixed in Phase 5-7).

---

## Phase 5 — React: API client + routing skeleton

**Files touched:** `internal/controller/ui/src/api.js` (new),
`internal/controller/ui/src/main.jsx`, `internal/controller/ui/package.json`
(add `react-router-dom`).

**Steps:**
1. `npm install react-router-dom` inside `internal/controller/ui/`.
2. `src/api.js`: thin `fetch` wrapper — `getStatus()`, `listActions()`,
   `getAction(id)`, `approveAction(id, token, secret, operator)`,
   `rejectAction(id, operator)`, `startCapacityLoad(params)`,
   `startBadPodLoad(params)`, `stopLoad()`, `getLoadStatus()`. All hit
   `/api/...` (relative — the Vite proxy from Phase 4 forwards to `:8080`
   in dev).
3. `main.jsx`: wrap `<App/>` in `<BrowserRouter>`, define routes for `/`,
   `/loadtest`, `/actions`, `/actions/:id`.

**Exit criteria:** `npm run dev`, navigate to each route in the browser —
even placeholder/empty pages should mount without console errors, confirms
routing + API client compile cleanly.

---

## Phase 6 — React: Dashboard page

**Files touched:** `internal/controller/ui/src/pages/Dashboard.jsx` (new).

**Steps:**
1. Poll `GET /api/status` every 3-5s (`setInterval` + cleanup on unmount).
2. Render cards: current/available/recommended replicas (highlight in red
   if they disagree), decision badge, SLI/P95/error rate/request rate,
   telemetry freshness (warn if stale beyond `freshness_limit` from config —
   expose that in `/api/status` too if not already), controller mode badge.

**Exit criteria:** with the controller port-forwarded on `:8080`, loading
`/` shows live numbers that change as you generate load (Phase 3's
endpoints, called via curl is fine for this phase's test — UI trigger comes
in Phase 7).

---

## Phase 7 — React: Load Test Control + Approvals/History pages

**Files touched:** `internal/controller/ui/src/pages/LoadTest.jsx` (new),
`internal/controller/ui/src/pages/Actions.jsx` (rewrite of current
`App.jsx`'s approval form, generalized).

**Steps (Load Test page):**
1. Two forms: Capacity (delayMs, vus, durationSeconds inputs + Start/Stop
   buttons calling `startCapacityLoad`/`stopLoad`), Bad Pod (errorRate,
   optional target pod dropdown populated from `/api/status`'s pod list if
   exposed, or free-text pod name).
2. Poll `GET /api/loadtest/status` every 3-5s while a job is active, show
   elapsed time / running state.

**Steps (Actions page):**
1. Fetch `GET /api/status` (or a dedicated "pending" field) to find the
   current actionable recommendation, if any — render the existing
   approve-form UI (decision, replicas, reason, expiry, operator input,
   Approve button) plus a new **Reject** button calling the Phase 2 reject
   endpoint.
2. Below it, fetch `GET /api/actions` and render a table: timestamp,
   decision, replicas before→after, outcome badge (approved/rejected/
   expired/superseded), operator (if approved/rejected).

**Exit criteria (this is also most of the manual E2E test — see the guide
below):** full loop — trigger capacity load from the UI, watch Dashboard
numbers rise, see a new pending recommendation appear on the Actions page,
approve it from the UI, watch Dashboard replicas climb, see it appear in
the history table as `approved`.

---

## Phase 8 — Cleanup

**Files touched/removed:** `scripts/load.sh`, `scripts/demo-capacity.sh`,
`scripts/demo-bad-pod.sh`, `scripts/demo-lib.sh` (all superseded by UI
buttons — delete), `DEMO.md` (rewrite to point at the React app instead of
scripts), root `Makefile` (`demo-load` target removed or repointed to "open
the UI"; `demo-ready` stays since infra bring-up is unchanged), `deploy/autopilot.yaml` (drop any UI-serving assumptions — likely none needed since it was embedded in the binary, not a separate manifest concern).

Keep: `scripts/k8s-setup.sh`, `scripts/physical-verify.sh`,
`scripts/physical-quarantine-verify.sh` — these test K8s mechanics directly,
unrelated to the demo-loop scripts being replaced.

**Exit criteria:** `grep -r "actions/latest\|demo-lib.sh" .` in the repo
returns nothing except this plan's own history/notes; `make demo-ready`
still works; README/DEMO.md accurately describe "open the React app, use
the Load Test page, approve in the Actions page."

---

# Full manual testing guide (run after each phase, and once fully at the end)

## Prerequisites (same as always)

```bash
# from repo root
make up                     # SigNoz stack
curl -sf http://localhost:8080/api/v1/health   # confirm SigNoz healthy
make k8s-setup               # kind cluster + KEDA (skip if already up)
```

```bash
cd incident-autopilot
source .env.local
go build -o bin/autopilot ./cmd/autopilot
./bin/autopilot install --config config.local.yaml --channel hackathon-email --approval-url http://localhost:8090
make k8s-build-images
make k8s-deploy
kubectl create secret generic incident-autopilot-approval --namespace autopilot-demo --from-literal=secret="${AUTOPILOT_APPROVAL_SECRET}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic signoz-credentials --namespace autopilot-demo --from-literal=api-key="${SIGNOZ_API_KEY}" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n autopilot-demo rollout restart deployment/incident-autopilot
kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s
kubectl -n autopilot-demo rollout status deployment/incident-autopilot --timeout=180s
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
```

**Expected:** `kubectl -n autopilot-demo get pods` shows 2 `checkout-api`
pods + 1 `incident-autopilot` pod, all `Running`/`1/1`.

```bash
kubectl -n autopilot-demo port-forward svc/incident-autopilot 18080:8080 &
```
**Expected:** `curl http://127.0.0.1:18080/metrics` returns Prometheus text
output. Keep this port-forward alive for the rest of the guide.

---

## Test 1 — Status API (validates Phase 1)

```bash
curl -s http://127.0.0.1:18080/api/status | jq
```
**Expected:** JSON with `currentReplicas: 2`, `recommendedReplicas: 2` (no
drift right after a clean baseline reset), `lastDecision: "hold"` (or
similar), populated `lastSnapshot` fields, `mode: "approval"`.

**Failure signs:** 404 → route not registered. Empty/zero snapshot fields →
SigNoz query path broken (check `telemetryFreshnessSeconds`, should be
small, e.g. <20s).

---

## Test 2 — History + reject (validates Phase 2)

1. Trigger a load test (via curl until Phase 7's UI exists — see Test 4)
   until a `scale_up` recommendation appears:
   ```bash
   curl -s http://127.0.0.1:18080/actions/latest -I   # find current rec id via Location header, or use /api/status
   ```
2. Reject it:
   ```bash
   curl -X POST http://127.0.0.1:18080/api/actions/<rec-id>/reject \
     -H "X-Autopilot-Operator: manual-test"
   ```
   **Expected:** `200 OK`, and the recommendation stops being "pending" —
   `autopilot_recommended_replicas` metric does NOT change (reject must
   never publish a scale change).
3. ```bash
   curl -s http://127.0.0.1:18080/api/actions | jq
   ```
   **Expected:** the rejected entry appears with `"outcome": "rejected"`.
4. Restart the controller pod (`kubectl -n autopilot-demo rollout restart
   deployment/incident-autopilot`), re-run the same curl.
   **Expected:** the rejected entry is still there (state persisted through
   restart).

---

## Test 3 — Load test API (validates Phase 3)

```bash
curl -X POST http://127.0.0.1:18080/api/loadtest/capacity \
  -H "Content-Type: application/json" \
  -d '{"delayMs":1500,"vus":40,"durationSeconds":300}'
```
**Expected:** `200 OK`. Then:
```bash
kubectl -n autopilot-demo get job load-generator
```
**Expected:** Job exists, pod `Running`.

```bash
curl -s http://127.0.0.1:18080/api/loadtest/status | jq
```
**Expected:** reports the job as active with elapsed time.

Wait ~1-2 minutes, poll `/api/status` — **expected:** `lastDecision` flips
to `scale_up` as SLI/P95 degrade under load.

```bash
curl -X POST http://127.0.0.1:18080/api/loadtest/stop
```
**Expected:** `200 OK`, Job deleted:
```bash
kubectl -n autopilot-demo get job load-generator   # should error "not found"
```

---

## Test 4 — Full UI loop (validates Phases 4-7, the real end-to-end test)

1. ```bash
   cd internal/controller/ui
   npm install
   npm run dev
   ```
   **Expected:** Vite prints a local URL (e.g. `http://localhost:5173`).
   Open it in a browser.

2. **Dashboard page (`/`):** should show live replica counts (2/2), decision
   `hold`, SLI near 100%, P95 low. Numbers should update every few seconds
   without a manual refresh.

3. **Load Test page (`/loadtest`):** fill in the Capacity form (delay
   1500ms, 40 VUs, 300s duration), click **Start**.
   **Expected:** status area shows "running", elapsed timer ticking.

4. Go back to **Dashboard** — within 1-2 minutes, **expected:** request
   rate and P95 latency visibly rise, decision eventually flips from `hold`
   to `scale_up`.

5. **Actions page (`/actions`):** **expected:** the pending `scale_up`
   recommendation appears automatically (no manual URL-guessing), showing
   decision, replica change (2 → N), reason text, expiry countdown.

6. Enter an operator name, click **Approve**.
   **Expected:** success message; within a few seconds, **Dashboard**
   shows `recommendedReplicas` and then `currentReplicas` climbing toward
   the approved target as KEDA reacts.

7. Scroll down on **Actions page** to the history table.
   **Expected:** the just-approved entry appears with outcome `approved`,
   correct before/after replica counts, and the operator name you typed.

8. Back on **Load Test page**, click **Stop**.
   **Expected:** job status flips to "not running"; injected pod delay
   clears (confirm via Dashboard — P95 should trend back down over the next
   couple of minutes as the queue drains).

9. **Bad-pod path:** on **Load Test page**, switch to the Bad Pod form,
   set error rate 100%, leave target pod blank (auto-pick), click **Start**.
   **Expected:** after ~1-2 min, Dashboard/Actions shows a
   `quarantine_replace` recommendation naming a specific pod. Approve it.
   **Expected:** that specific pod disappears from
   `kubectl -n autopilot-demo get pods` (drained + replaced), history table
   logs it as `approved` with the target pod name visible.

10. **Reject path:** trigger another capacity load, when a `scale_up`
    recommendation appears, click **Reject** instead of Approve.
    **Expected:** Dashboard's `recommendedReplicas` stays unchanged (no
    scale action taken), history table shows outcome `rejected`.

---

## Test 5 — Drift/stale-state regression check

This directly re-tests the bug that caused today's confusion (stale
`autopilot_recommended_replicas` fighting manual `kubectl scale`).

1. Approve a scale-up to 6 replicas (Test 4 steps 3-6).
2. Without using the UI or approving a scale-down, run:
   ```bash
   kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
   ```
3. **Expected:** within ~15-60s (KEDA polling interval), replicas bounce
   back to 6 — and critically, **Dashboard should visibly show the drift**
   (`currentReplicas` briefly reads 2, `recommendedReplicas` reads 6, drift
   indicator lit) rather than silently looking "fine." This confirms the
   Phase 1 status API surfaces exactly the condition that was invisible
   before.
4. To actually resolve it correctly: trigger/approve a `scale_down`
   recommendation through the UI (don't fight it with raw `kubectl scale`)
   and confirm the Dashboard settles with no drift once KEDA converges.

---

## Cleanup after any test run

```bash
curl -X POST http://127.0.0.1:18080/api/loadtest/stop
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2   # only after approving/letting a real scale-down settle, not via force
```
