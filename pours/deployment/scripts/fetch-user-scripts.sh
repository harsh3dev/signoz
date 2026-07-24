#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST="${ROOT}/telemetrystore/user-scripts"
BINARY="${DEST}/histogramQuantile"
VERSION="v0.0.1"

if [[ -x "${BINARY}" ]]; then
  echo "ClickHouse user script already present: ${BINARY}"
  exit 0
fi

mkdir -p "${DEST}"

# Containers often cannot verify TLS behind corporate proxies; fetch on the host instead.
node_os="linux"
case "$(uname -m)" in
  aarch64|arm64) node_arch="arm64" ;;
  x86_64|amd64) node_arch="amd64" ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

archive="$(mktemp)"
trap 'rm -f "${archive}"' EXIT

echo "Fetching histogram-quantile ${VERSION} for ${node_os}/${node_arch}"
curl -fsSL \
  -o "${archive}" \
  "https://github.com/SigNoz/signoz/releases/download/histogram-quantile%2F${VERSION}/histogram-quantile_${node_os}_${node_arch}.tar.gz"

tar -xzf "${archive}" -C "${DEST}"
mv "${DEST}/histogram-quantile" "${BINARY}"
chmod +x "${BINARY}"

echo "Installed ${BINARY}"
