#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=demo-lib.sh
source "${SCRIPT_DIR}/demo-lib.sh"

CAPACITY_DELAY_MS="${CAPACITY_DELAY_MS:-1500}"
CAPACITY_TARGET_REPLICAS="${CAPACITY_TARGET_REPLICAS:-6}"
K6_VUS="${K6_VUS:-40}"
K6_DURATION="${K6_DURATION:-15m}"

echo "==> SigNoz Incident Autopilot — capacity pressure demo"
demo_prereqs
ensure_context
reset_demo_baseline 2

echo "==> Step 1: confirm baseline replicas"
wait_for_ready_replicas 2 120

echo "==> Step 2: start deterministic traffic"
apply_load_generator "${K6_VUS}" "${K6_DURATION}"
while IFS= read -r pod; do
  [[ -z "${pod}" ]] && continue
  echo "    setting inventory delay ${CAPACITY_DELAY_MS}ms on ${pod}"
  pod_http_post "${pod}" "{\"inventoryDelayMs\":${CAPACITY_DELAY_MS}}"
done < <(list_ready_pods)

rec_id="$(wait_for_recommendation scale_up)"
echo "==> Step 3-4: approval recommendation detected"
print_recommendation "${rec_id}"

echo "==> Step 5: pause for operator approval"
pause_or_auto_approve "${rec_id}"

echo "==> Step 6: wait for scale-up"
wait_for_ready_replicas "${CAPACITY_TARGET_REPLICAS}" "${DEMO_WAIT_TIMEOUT}"

echo "==> Step 7: verification result"
wait_for_verification_log "$((DEMO_WAIT_TIMEOUT + 180))"
query_signoz_sli

echo ""
echo "Capacity demo complete."
echo "Expected: checkout-api scaled from 2 to ${CAPACITY_TARGET_REPLICAS} after approval."
