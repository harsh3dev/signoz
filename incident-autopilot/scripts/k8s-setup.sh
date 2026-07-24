#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="$(cd "$ROOT/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-autopilot}"
KEDA_NAMESPACE="${KEDA_NAMESPACE:-keda}"
KEDA_RELEASE="${KEDA_RELEASE:-keda}"

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required tool: $1" >&2
    exit 1
  fi
}

echo "==> Checking prerequisites"
for tool in docker kubectl kind helm; do
  require "$tool"
done

if ! docker info >/dev/null 2>&1; then
  echo "Docker is not running; start Docker Desktop and retry." >&2
  exit 1
fi

echo "==> Ensuring Kind cluster '$CLUSTER_NAME'"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  kind create cluster --name "$CLUSTER_NAME" --config "$ROOT/deploy/kind.yaml"
fi
kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

echo "==> Installing KEDA"
helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update kedacore
helm upgrade --install "$KEDA_RELEASE" kedacore/keda \
  --namespace "$KEDA_NAMESPACE" \
  --create-namespace \
  --wait \
  --timeout 5m

echo "==> Verifying cluster"
kubectl get nodes
kubectl -n "$KEDA_NAMESPACE" rollout status deployment/keda-operator --timeout=120s
kubectl get crd scaledobjects.keda.sh >/dev/null

echo ""
echo "Kubernetes test environment is ready."
echo "  context: kind-$CLUSTER_NAME"
echo "  next:    cd $REPO_ROOT && make k8s-build-images k8s-deploy"
