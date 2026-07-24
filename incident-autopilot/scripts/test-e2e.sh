#!/usr/bin/env bash
# One-shot runner for the full incident-autopilot test flow.
# Idempotent: safe to re-run from scratch even if SigNoz/kind/images already exist.
#
# Usage:
#   ./scripts/test-e2e.sh              # everything: setup + unit tests + both demos (manual approve)
#   ./scripts/test-e2e.sh setup        # just get to a running baseline (SigNoz, kind, deploy)
#   ./scripts/test-e2e.sh unit         # go test/vet only
#   ./scripts/test-e2e.sh capacity     # capacity demo only (assumes setup done)
#   ./scripts/test-e2e.sh badpod       # bad-pod demo only (assumes setup done)
#   DEMO_AUTO_APPROVE=true ./scripts/test-e2e.sh   # unattended, no manual approve prompts
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
AUTOPILOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${AUTOPILOT_DIR}/.." && pwd)"

# nvm shell functions in this repo's dev environment are broken (_nvm_lazy_load
# missing); use the real node/npm binaries directly instead of the shell fn.
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

run_unit() {
  step "Unit tests"
  cd "${AUTOPILOT_DIR}"
  make ui-build
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
    --approval-url http://localhost:8090

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
  step "Capacity incident demo"
  cd "${AUTOPILOT_DIR}"
  make demo-capacity
}

run_badpod() {
  load_env
  step "Bad-pod quarantine demo"
  cd "${AUTOPILOT_DIR}"
  kubectl -n autopilot-demo scale deployment/checkout-api --replicas=2
  make demo-bad-pod
}

case "${1:-all}" in
  unit)     run_unit ;;
  setup)    run_setup ;;
  capacity) run_capacity ;;
  badpod)   run_badpod ;;
  all)
    run_unit
    run_setup
    run_capacity
    run_badpod
    step "All done"
    ;;
  *)
    echo "usage: $0 [unit|setup|capacity|badpod|all]" >&2
    exit 1
    ;;
esac
