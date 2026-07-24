#!/usr/bin/env bash
# Generates load + injects latency so a scale_up recommendation appears.
# Run this, then open http://127.0.0.1:18080/actions/latest to approve.
set -euo pipefail

NAMESPACE="${NAMESPACE:-autopilot-demo}"
DEPLOYMENT="${DEPLOYMENT:-checkout-api}"
DELAY_MS="${DELAY_MS:-1500}"
VUS="${VUS:-40}"
DURATION="${DURATION:-15m}"
DURATION_MS=$(( ${DURATION%m} * 60 * 1000 ))

echo "==> setting inventory delay ${DELAY_MS}ms on all ${DEPLOYMENT} pods"
for pod in $(kubectl -n "${NAMESPACE}" get pods -l "app=${DEPLOYMENT}" -o jsonpath='{.items[*].metadata.name}'); do
  kubectl -n "${NAMESPACE}" exec "${pod}" -- env PAYLOAD="{\"inventoryDelayMs\":${DELAY_MS}}" node -e '
const http=require("http");
const p=JSON.parse(process.env.PAYLOAD);const b=JSON.stringify(p);
const r=http.request({hostname:"127.0.0.1",port:3000,path:"/api/demo/behavior",method:"POST",headers:{"Content-Type":"application/json","Content-Length":Buffer.byteLength(b)}},res=>{res.resume();process.exit(0)});
r.on("error",()=>process.exit(1));r.write(b);r.end();'
  echo "    ${pod} ok"
done

echo "==> starting load generator (vus=${VUS}, duration=${DURATION})"
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
              value: "${VUS}"
            - name: DURATION_MS
              value: "${DURATION_MS}"
            - name: TARGET_URL
              value: http://checkout-api.${NAMESPACE}.svc.cluster.local:3000/api/orders
          command: ["node", "-e"]
          args:
            - |
              const http = require("http");
              const url = process.env.TARGET_URL;
              const duration = Number(process.env.DURATION_MS || 900000);
              const concurrency = Number(process.env.CONCURRENCY || 20);
              const payload = JSON.stringify({items:[{id:"prod-001",quantity:1}],customerName:"load-generator",shippingAddress:"1 Autopilot Way"});
              const deadline = Date.now() + duration;
              const worker = () => {
                if (Date.now() >= deadline) return;
                const req = http.request(url,{method:"POST",headers:{"Content-Type":"application/json","Content-Length":Buffer.byteLength(payload)}},(res)=>{res.resume();setImmediate(worker);});
                req.on("error", () => setImmediate(worker));
                req.write(payload); req.end();
              };
              for (let i = 0; i < concurrency; i++) worker();
              setTimeout(() => process.exit(0), duration + 2000);
EOF

AUTOPILOT_URL="${AUTOPILOT_URL:-http://127.0.0.1:18080}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-300}"

echo ""
echo "==> waiting for a scale_up recommendation (checks every 5s, timeout ${WAIT_TIMEOUT}s)"
start=$(date +%s)
while true; do
  headers="$(curl -sI "${AUTOPILOT_URL}/actions/latest" 2>/dev/null || true)"
  if echo "${headers}" | head -1 | grep -qE '302|301'; then
    rec_id="$(echo "${headers}" | awk -F': ' 'tolower($1)=="location"{print $2}' | tr -d '\r' | xargs -n1 basename)"
    decision="$(curl -sf "${AUTOPILOT_URL}/api/actions/${rec_id}" 2>/dev/null | jq -r '.decision // empty')"
    now_ts=$(date +%T)
    echo "    [${now_ts}] rec=${rec_id} decision=${decision:-pending}"
    if [[ "${decision}" == "scale_up" || "${decision}" == "quarantine_replace" ]]; then
      echo ""
      echo "############################################################"
      echo "  READY FOR APPROVAL (${decision})"
      echo "  ${AUTOPILOT_URL}/actions/${rec_id}"
      echo "############################################################"
      echo ""
      break
    fi
  fi
  if (( $(date +%s) - start >= WAIT_TIMEOUT )); then
    echo "    timed out waiting for scale_up after ${WAIT_TIMEOUT}s; keep checking ${AUTOPILOT_URL}/actions/latest manually"
    break
  fi
  sleep 5
done

echo "Stop load early with: kubectl -n ${NAMESPACE} delete job load-generator"
