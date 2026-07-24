#!/usr/bin/env bash
# Shared helpers for incident-autopilot demo scripts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-autopilot}"
NAMESPACE="${NAMESPACE:-autopilot-demo}"
DEPLOYMENT="${DEPLOYMENT:-checkout-api}"
AUTOPILOT_SERVICE="${AUTOPILOT_SERVICE:-incident-autopilot}"
AUTOPILOT_LOCAL_PORT="${AUTOPILOT_LOCAL_PORT:-18080}"
DEMO_WAIT_TIMEOUT="${DEMO_WAIT_TIMEOUT:-600}"
DEMO_POLL_INTERVAL="${DEMO_POLL_INTERVAL:-5}"
DEMO_AUTO_APPROVE="${DEMO_AUTO_APPROVE:-false}"
AUTOPILOT_APPROVAL_SECRET="${AUTOPILOT_APPROVAL_SECRET:-}"

PF_PID=""

cleanup() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 1
  fi
}

demo_prereqs() {
  for tool in kubectl curl jq; do
    require_cmd "$tool"
  done
}

ensure_context() {
  local ctx="kind-${CLUSTER_NAME}"
  if ! kubectl config get-contexts -o name 2>/dev/null | grep -qx "${ctx}"; then
    echo "kubectl context ${ctx} not found; run: make cluster" >&2
    exit 1
  fi
  kubectl config use-context "${ctx}" >/dev/null
}

diag_replicas() {
  cat <<EOF
Diagnostics:
  kubectl -n ${NAMESPACE} get deploy ${DEPLOYMENT}
  kubectl -n ${NAMESPACE} get pods -l app=${DEPLOYMENT} -o wide
  kubectl -n ${NAMESPACE} describe hpa
  kubectl -n ${NAMESPACE} logs deploy/${AUTOPILOT_SERVICE} --tail=80
EOF
}

diag_endpoints() {
  cat <<EOF
Diagnostics:
  kubectl -n ${NAMESPACE} get endpointslices -l kubernetes.io/service-name=${DEPLOYMENT}
  kubectl -n ${NAMESPACE} get pods -l app=${DEPLOYMENT} -o wide
  kubectl -n ${NAMESPACE} logs deploy/${AUTOPILOT_SERVICE} --tail=80
EOF
}

ready_replica_count() {
  kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
    -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo 0
}

wait_for_ready_replicas() {
  local want="$1"
  local timeout="${2:-${DEMO_WAIT_TIMEOUT}}"
  local start
  start=$(date +%s)
  echo "==> Waiting for ${want} ready replicas of ${DEPLOYMENT}"
  while true; do
    local got
    got="$(ready_replica_count)"
    if [[ -z "${got}" ]]; then
      got=0
    fi
    if [[ "${got}" -ge "${want}" ]]; then
      echo "    ${got}/${want} replicas ready"
      return 0
    fi
    if (( $(date +%s) - start >= timeout )); then
      echo "timed out waiting for ${want} ready replicas (have ${got})" >&2
      diag_replicas
      return 1
    fi
    sleep "${DEMO_POLL_INTERVAL}"
  done
}

start_autopilot_port_forward() {
  if [[ -n "${PF_PID}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
    return 0
  fi
  kubectl -n "${NAMESPACE}" port-forward "svc/${AUTOPILOT_SERVICE}" \
    "${AUTOPILOT_LOCAL_PORT}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${AUTOPILOT_LOCAL_PORT}/metrics" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "autopilot port-forward failed on localhost:${AUTOPILOT_LOCAL_PORT}" >&2
  diag_replicas
  return 1
}

autopilot_base_url() {
  echo "http://127.0.0.1:${AUTOPILOT_LOCAL_PORT}"
}

load_approval_secret() {
  if [[ -n "${AUTOPILOT_APPROVAL_SECRET}" ]]; then
    return 0
  fi
  if ! kubectl -n "${NAMESPACE}" get secret incident-autopilot-approval >/dev/null 2>&1; then
    echo "approval secret not found; create incident-autopilot-approval or set AUTOPILOT_APPROVAL_SECRET" >&2
    return 1
  fi
  AUTOPILOT_APPROVAL_SECRET="$(
    kubectl -n "${NAMESPACE}" get secret incident-autopilot-approval \
      -o jsonpath='{.data.secret}' | base64 -d
  )"
}

pending_recommendation_id() {
  local headers status location
  headers="$(curl -sI "$(autopilot_base_url)/actions/latest" 2>/dev/null || true)"
  status="$(echo "${headers}" | head -1)"
  if ! echo "${status}" | grep -qE '302|301'; then
    return 1
  fi
  location="$(echo "${headers}" | awk -F': ' 'tolower($1)=="location"{print $2}' | tr -d '\r')"
  basename "${location:-}"
}

recommendation_decision() {
  local rec_id="$1"
  local page
  page="$(curl -sf "$(autopilot_base_url)/actions/${rec_id}")"
  echo "${page}" | sed -n 's/.*<strong>Decision:<\/strong> \([^<]*\).*/\1/p' | head -1 | tr -d '[:space:]'
}

wait_for_recommendation() {
  local decision="$1"
  local timeout="${2:-${DEMO_WAIT_TIMEOUT}}"
  local start
  start=$(date +%s)
  echo "==> Waiting for ${decision} recommendation"
  start_autopilot_port_forward
  while true; do
    local rec_id got
    if rec_id="$(pending_recommendation_id)"; then
      got="$(recommendation_decision "${rec_id}")"
      if [[ "${got}" == "${decision}" ]]; then
        echo "${rec_id}"
        return 0
      fi
    fi
    if (( $(date +%s) - start >= timeout )); then
      echo "timed out waiting for ${decision} recommendation" >&2
      diag_replicas
      return 1
    fi
    sleep "${DEMO_POLL_INTERVAL}"
  done
}

print_recommendation() {
  local rec_id="$1"
  start_autopilot_port_forward
  local url
  url="$(autopilot_base_url)/actions/${rec_id}"
  echo ""
  echo "Recommendation ready:"
  echo "  id:      ${rec_id}"
  echo "  approve: ${url}"
  curl -sf "${url}" | sed -n '1,/<form/p' | sed '/<form/d' | sed 's/<[^>]*>//g' | sed '/^[[:space:]]*$/d' | sed 's/^/  /'
}

approve_recommendation() {
  local rec_id="$1"
  local operator="${2:-demo-operator}"
  load_approval_secret
  start_autopilot_port_forward
  local page token
  page="$(curl -sf "$(autopilot_base_url)/actions/${rec_id}")"
  token="$(echo "${page}" | sed -n 's/.*name="token" value="\([^"]*\)".*/\1/p' | head -1)"
  if [[ -z "${token}" ]]; then
    echo "failed to extract approval token for ${rec_id}" >&2
    return 1
  fi
  curl -sf -X POST "$(autopilot_base_url)/api/actions/${rec_id}/approve" \
    -H "Authorization: Bearer ${AUTOPILOT_APPROVAL_SECRET}" \
    -H "X-Autopilot-Operator: ${operator}" \
    -d "token=${token}" >/dev/null
  echo "==> Approved recommendation ${rec_id} as ${operator}"
}

pause_or_auto_approve() {
  local rec_id="$1"
  if [[ "${DEMO_AUTO_APPROVE}" == "true" ]]; then
    approve_recommendation "${rec_id}"
    return 0
  fi
  echo ""
  echo "Pause: open the approval URL above and approve the action."
  echo "Press Enter after approval, or set DEMO_AUTO_APPROVE=true to approve automatically."
  read -r _
}

pod_http_post() {
  local pod="$1"
  local payload="$2"
  kubectl exec -n "${NAMESPACE}" "${pod}" -- env PAYLOAD="${payload}" node -e '
const http = require("http");
const payload = JSON.parse(process.env.PAYLOAD);
const body = JSON.stringify(payload);
const req = http.request(
  {
    hostname: "127.0.0.1",
    port: 3000,
    path: "/api/demo/behavior",
    method: "POST",
    headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) },
  },
  (res) => {
    res.resume();
    process.exit(res.statusCode >= 200 && res.statusCode < 300 ? 0 : 1);
  },
);
req.on("error", () => process.exit(1));
req.write(body);
req.end();
'
}

list_ready_pods() {
  kubectl -n "${NAMESPACE}" get pods -l "app=${DEPLOYMENT}" -o json \
    | jq -r '.items[] | select(any(.status.conditions[]?; .type == "Ready" and .status == "True")) | .metadata.name'
}

apply_load_generator() {
  local vus="${1:-40}"
  local duration="${2:-15m}"
  local duration_ms=900000
  case "${duration}" in
    *m) duration_ms=$(( ${duration%m} * 60 * 1000 )) ;;
    *s) duration_ms=$(( ${duration%s} * 1000 )) ;;
  esac

  kubectl -n "${NAMESPACE}" delete job load-generator --ignore-not-found >/dev/null 2>&1 || true
  cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: load-generator
  namespace: ${NAMESPACE}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: loader
          image: telemetry-shop:dev
          imagePullPolicy: IfNotPresent
          env:
            - name: CONCURRENCY
              value: "${vus}"
            - name: DURATION_MS
              value: "${duration_ms}"
            - name: TARGET_URL
              value: http://checkout-api.${NAMESPACE}.svc.cluster.local:3000/api/orders
          command: ["node", "-e"]
          args:
            - |
              const http = require("http");
              const url = process.env.TARGET_URL;
              const duration = Number(process.env.DURATION_MS || 900000);
              const concurrency = Number(process.env.CONCURRENCY || 20);
              const payload = JSON.stringify({
                items: [{ id: "prod-001", quantity: 1 }],
                customerName: "load-generator",
                shippingAddress: "1 Autopilot Way",
              });
              const deadline = Date.now() + duration;
              const worker = () => {
                if (Date.now() >= deadline) return;
                const req = http.request(
                  url,
                  {
                    method: "POST",
                    headers: {
                      "Content-Type": "application/json",
                      "Content-Length": Buffer.byteLength(payload),
                    },
                  },
                  (res) => {
                    res.resume();
                    setImmediate(worker);
                  },
                );
                req.on("error", () => setImmediate(worker));
                req.write(payload);
                req.end();
              };
              for (let i = 0; i < concurrency; i++) worker();
              setTimeout(() => process.exit(0), duration + 2000);
EOF

  echo "==> Waiting for load-generator pod"
  local start
  start=$(date +%s)
  while true; do
    local phase
    phase="$(kubectl -n "${NAMESPACE}" get pods -l job-name=load-generator \
      -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    if [[ "${phase}" == "Running" ]]; then
      break
    fi
    if [[ "${phase}" == "Failed" || "${phase}" == "Succeeded" ]]; then
      kubectl -n "${NAMESPACE}" describe pod -l job-name=load-generator | tail -20 >&2
      return 1
    fi
    if (( $(date +%s) - start >= 120 )); then
      kubectl -n "${NAMESPACE}" describe pod -l job-name=load-generator | tail -20 >&2
      echo "timed out waiting for load-generator pod" >&2
      return 1
    fi
    sleep 2
  done
  echo "==> Started load generator (concurrency=${vus}, duration=${duration})"
}

reset_demo_baseline() {
  local replicas="${1:-2}"
  echo "==> Resetting demo baseline to ${replicas} replicas"
  kubectl -n "${NAMESPACE}" delete job load-generator --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" annotate scaledobject checkout-api-autopilot \
    autoscaling.keda.sh/paused-replicas="${replicas}" \
    autoscaling.keda.sh/paused=true --overwrite >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" scale deployment/"${DEPLOYMENT}" --replicas="${replicas}"
  kubectl -n "${NAMESPACE}" rollout status deployment/"${DEPLOYMENT}" --timeout=180s
  kubectl -n "${NAMESPACE}" delete pod -l app="${AUTOPILOT_SERVICE}" --wait=true
  kubectl -n "${NAMESPACE}" rollout status deployment/"${AUTOPILOT_SERVICE}" --timeout=120s
  kubectl -n "${NAMESPACE}" annotate scaledobject checkout-api-autopilot \
    autoscaling.keda.sh/paused- --overwrite >/dev/null 2>&1 || true
  kubectl -n "${NAMESPACE}" annotate scaledobject checkout-api-autopilot \
    autoscaling.keda.sh/paused-replicas- --overwrite >/dev/null 2>&1 || true
  while IFS= read -r pod; do
    [[ -z "${pod}" ]] && continue
    pod_http_post "${pod}" '{"inventoryDelayMs":0,"inventoryErrorRate":0}' >/dev/null 2>&1 || true
  done < <(list_ready_pods 2>/dev/null || true)
  wait_for_ready_replicas "${replicas}" 180
}

wait_for_verification_log() {
  local timeout="${1:-${DEMO_WAIT_TIMEOUT}}"
  local start
  start=$(date +%s)
  echo "==> Waiting for post-action verification"
  start_autopilot_port_forward
  while true; do
    local published
    published="$(curl -sf "$(autopilot_base_url)/metrics" \
      | awk '/^autopilot_recommended_replicas\{/{print $2; exit}')"
    if [[ -n "${published}" ]] && [[ "${published%.*}" -ge 2 ]]; then
      echo "    autopilot published replicas: ${published%.*}"
    fi

    local line
    line="$(kubectl -n "${NAMESPACE}" logs "deploy/${AUTOPILOT_SERVICE}" --tail=200 2>/dev/null \
      | grep -E 'autopilot\.result|incident_report|recovered|ineffective|rollout_failed' | tail -1 || true)"
    if [[ -n "${line}" ]]; then
      echo "    ${line}"
      if echo "${line}" | grep -qi 'recovered'; then
        return 0
      fi
      if echo "${line}" | grep -Eqi 'ineffective|rollout_failed'; then
        echo "verification did not recover" >&2
        diag_replicas
        return 1
      fi
    fi

    local available target
    available="$(ready_replica_count)"
    target="$(kubectl -n "${NAMESPACE}" get deployment "${DEPLOYMENT}" \
      -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)"
    if [[ -n "${available}" && -n "${target}" && "${available}" -ge "${target}" && "${target}" -ge 2 ]]; then
      echo "    verification window complete: ${available}/${target} replicas ready"
      return 0
    fi

    if (( $(date +%s) - start >= timeout )); then
      echo "timed out waiting for verification" >&2
      diag_replicas
      return 1
    fi
    sleep "${DEMO_POLL_INTERVAL}"
  done
}

query_signoz_sli() {
  local api_key="${SIGNOZ_API_KEY:-}"
  local signoz_url="${SIGNOZ_URL:-http://localhost:8080}"
  if [[ -z "${api_key}" ]]; then
    echo "    (set SIGNOZ_API_KEY to query recovered SLI from SigNoz)"
    return 0
  fi
  local end start payload
  end=$(date +%s)
  start=$((end - 300))
  payload="$(jq -n \
    --arg q '1 - ((sum(rate(checkout_requests_total{"service.name"="checkout-api",status="failed"}[5m])) or sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[5m])) * 0) / sum(rate(checkout_requests_total{"service.name"="checkout-api",status="success"}[5m])))' \
    --argjson start "${start}" \
    --argjson end "${end}" \
    '{query:$q,start:$start,end:$end,step:60}')"
  local result
  result="$(curl -sf "${signoz_url}/api/v5/query_range" \
    -H "Content-Type: application/json" \
    -H "SIGNOZ-API-KEY: ${api_key}" \
    -d "${payload}" 2>/dev/null || true)"
  if [[ -z "${result}" ]]; then
    echo "    SigNoz SLI query failed (is SigNoz up at ${signoz_url}?)"
    return 0
  fi
  local sli
  sli="$(echo "${result}" | jq -r '.data.result[0].series[0].values[-1].value // empty' 2>/dev/null || true)"
  if [[ -n "${sli}" ]]; then
    printf "    recovered SLI (SigNoz): %.3f\n" "${sli}"
  else
    echo "    SigNoz SLI query returned no recent points"
  fi
}

pod_in_endpoints() {
  local pod_uid="$1"
  kubectl -n "${NAMESPACE}" get endpointslices \
    -l "kubernetes.io/service-name=${DEPLOYMENT}" \
    -o json | jq -e --arg uid "${pod_uid}" '
      .items[].endpoints[]? | select(.targetRef.uid == $uid)
    ' >/dev/null 2>&1
}

wait_until_not_routed() {
  local pod_name="$1"
  local timeout="${2:-${DEMO_WAIT_TIMEOUT}}"
  local pod_uid
  pod_uid="$(kubectl -n "${NAMESPACE}" get pod "${pod_name}" -o jsonpath='{.metadata.uid}')"
  local start
  start=$(date +%s)
  echo "==> Waiting for ${pod_name} to leave EndpointSlices"
  while true; do
    if ! pod_in_endpoints "${pod_uid}"; then
      echo "    ${pod_name} drained from service endpoints"
      return 0
    fi
    if (( $(date +%s) - start >= timeout )); then
      echo "timed out waiting for ${pod_name} to drain" >&2
      diag_endpoints
      return 1
    fi
    sleep "${DEMO_POLL_INTERVAL}"
  done
}

pick_target_pod() {
  list_ready_pods | sort | tail -1
}
