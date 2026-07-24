#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-autopilot}"
export PATH="$(go env GOPATH)/bin:$PATH"

echo "==> Ensuring kind cluster '$CLUSTER_NAME'"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  kind create cluster --name "$CLUSTER_NAME" --config "$ROOT/deploy/kind.yaml"
fi
kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

echo "==> Building demo app image telemetry-shop:dev"
docker build -t telemetry-shop:dev "$REPO_ROOT/demo-app"

echo "==> Loading image into kind"
kind load docker-image telemetry-shop:dev --name "$CLUSTER_NAME"

echo "==> Deploying checkout-api"
kubectl apply -f "$ROOT/deploy/demo-app.yaml"
kubectl -n autopilot-demo rollout status deployment/checkout-api --timeout=180s

echo "==> Running physical verification test"
cd "$ROOT"
go run ./cmd/physical-test \
  --config config.example.yaml \
  --scale-to 4 \
  --otlp-endpoint http://localhost:4318

echo "==> Done"
