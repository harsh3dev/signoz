#!/usr/bin/env bash
# One-shot runner for the full incident-autopilot test flow.
# Idempotent: safe to re-run from scratch even if SigNoz/kind/images already exist.
#
# Usage:
#   ./scripts/test-e2e.sh              # everything: setup + unit tests
#   ./scripts/test-e2e.sh setup        # just get to a running baseline
#   ./scripts/test-e2e.sh unit         # go test/vet only
#   ./scripts/test-e2e.sh capacity     # start capacity load via API (approve in UI)
#   ./scripts/test-e2e.sh badpod       # start bad-pod load via API (approve in UI)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
AUTOPILOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${AUTOPILOT_DIR}/.." && pwd)"
API_URL="${AUTOPILOT_API_URL:-http://127.0.0.1:8090}"

if [[ -d "${HOME}/.nvm/versions/node" ]]; then
  NODE_BIN="$(ls -d "${HOME}"/.nvm/versions/node/*/bin 2>/dev/null | sort -V | tail -1)"
  [[ -n "${NODE_BIN}" ]] && export PATH="${NODE_BIN}:${PATH}"
fi
unset -f node npm 2>/dev/null || true

AUTOPILOT_SECRET="${AUTOPILOT_APPROVAL_SECRET:-dev-approval-secret}"
export AUTOPILOT_APPROVAL_SECRET="${AUTOPILOT_SECRET}"

step() { echo ""; echo "=== $* ==="; }

load_env() {
  if [[ -f "${AUTOPILOT_DIR}/.env.local" ]]; then
    set -a
    # shellcheck disable=SC1091
    source "${AUTOPILOT_DIR}/.env.local"
    set +a
  fi
}

ensure_port_forward() {
  if curl -sf "${API_URL}/metrics" >/dev/null 2>&1; then
    return 0
  fi
  echo "==> starting port-forward ${API_URL}"
  kubectl -n autopilot-demo port-forward svc/incident-autopilot 8090:8080 >/tmp/autopilot-pf.log 2>&1 &
  sleep 2
  curl -sf "${API_URL}/metrics" >/dev/null
}

run_unit() {
  step "Unit tests"
  cd "${AUTOPILOT_DIR}"
  go test ./...
  go vet ./...
}

run_setup() {
  load_env
  if [[ -z "${SIGNOZ_API_KEY:-}" ]]; then
    echo "SIGNOZ_API_KEY is not set (put it in incident-autopilot/.env.local)." >&2
    exit 1
  fi

  step "SigNoz stack"
  cd "${REPO_ROOT}"
  if curl -sf http://localhost:8080/api/v1/health >/dev/null 2>&1; then
    echo "already up"
  else
    make up
  fi

  step "Dashboard + alerts install"
  cd "${AUTOPILOT_DIR}"
  go build -o bin/autopilot ./cmd/autopilot
  ./bin/autopilot install \
    --config config.local.yaml \
    --channel hackathon-email \
    --approval-url http://localhost:5173

  step "Kind cluster + KEDA"
  cd "${REPO_ROOT}"
  if kind get clusters 2>/dev/null | grep -qx autopilot; then
    kubectl config use-context kind-autopilot >/dev/null
    echo "kind cluster already exists"
  else
    make k8s-setup
  fi

  step "Build + load images"
  make k8s-build-images

  step "Deploy manifests"
  make k8s-deploy

  step "Secrets"
  kubectl create secret generic incident-autopilot-approval \
    --namespace autopilot-demo \
    --from-literal=secret="${AUTOPILOT_SECRET}" \
    --dry-run=client -o yaml | kubectl apply -f -

  kubectl create secret generic signoz-credentials \
    --namespace autopilot-demo \
    --from-literal=api-key="${SIGNOZ_API_KEY}" \
    --dry-run=client -o yaml | kubectl apply -f -

  kubectl -n autopilot-demo rollout restart deployment/incident-autopilot
  kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s
  kubectl -n autopilot-demo rollout status deployment/incident-autopilot --timeout=180s

  step "Baseline ready"
  kubectl -n autopilot-demo get pods
}

run_capacity() {
  load_env
  step "Capacity load (API)"
  ensure_port_forward
  curl -sf -X POST "${API_URL}/api/loadtest/capacity" \
    -H 'Content-Type: application/json' \
    -d '{"delayMs":1500,"vus":40,"durationSeconds":300}'
  echo ""
  echo "Load started. Open http://localhost:5173/actions to approve when scale_up appears."
}

run_badpod() {
  load_env
  step "Bad-pod load (API)"
  kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
  ensure_port_forward
  curl -sf -X POST "${API_URL}/api/loadtest/badpod" \
    -H 'Content-Type: application/json' \
    -d '{"errorRate":100}'
  echo ""
  echo "Bad-pod test started. Open http://localhost:5173/actions to approve quarantine_replace."
}

case "${1:-all}" in
  unit)     run_unit ;;
  setup)    run_setup ;;
  capacity) run_capacity ;;
  badpod)   run_badpod ;;
  all)
    run_unit
    run_setup
    step "All done — start UI: cd internal/controller/ui && npm run dev"
    ;;
  *)
    echo "usage: $0 [unit|setup|capacity|badpod|all]" >&2
    exit 1
    ;;
esac
