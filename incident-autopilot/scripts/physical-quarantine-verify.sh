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

echo "==> Loading image into kind (skip if already present)"
kind load docker-image telemetry-shop:dev --name "$CLUSTER_NAME" 2>/dev/null || echo "    skipped image load; using cluster image if present"

echo "==> Deploying checkout-api with readiness gates"
kubectl apply -f "$ROOT/deploy/demo-app.yaml"
kubectl -n autopilot-demo scale deployment/checkout-api --replicas=3

echo "==> Removing legacy ReplicaSets without readiness gates"
for rs in $(kubectl -n autopilot-demo get rs -l app=checkout-api -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.template.spec.readinessGates}{"\n"}{end}' | awk '$2 == "" {print $1}'); do
  kubectl -n autopilot-demo delete rs "$rs" --cascade=orphan 2>/dev/null || true
done
kubectl -n autopilot-demo delete pod -l app=checkout-api --wait=false 2>/dev/null || true
sleep 15
kubectl -n autopilot-demo get pods

echo "==> Running physical quarantine verification (syncs readiness gates before waiting)"
cd "$ROOT"
go run ./cmd/physical-quarantine-test \
  --config config.example.yaml \
  --otlp-endpoint http://localhost:4318

echo "==> Done"
