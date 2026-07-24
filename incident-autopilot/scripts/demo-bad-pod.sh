#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=demo-lib.sh
source "${SCRIPT_DIR}/demo-lib.sh"

BAD_POD_ERROR_RATE="${BAD_POD_ERROR_RATE:-100}"
OUTLIER_EVALUATIONS="${OUTLIER_EVALUATIONS:-3}"
OUTLIER_WAIT_SECONDS="${OUTLIER_WAIT_SECONDS:-$((OUTLIER_EVALUATIONS * 20 + 30))}"

echo "==> SigNoz Incident Autopilot — bad-pod quarantine demo"
demo_prereqs
ensure_context
reset_demo_baseline 2

echo "==> Step 1: select one owned ready pod"
target_pod="$(pick_target_pod)"
if [[ -z "${target_pod}" ]]; then
  echo "no ready checkout-api pod found" >&2
  diag_replicas
  exit 1
fi
target_uid="$(kubectl -n "${NAMESPACE}" get pod "${target_pod}" -o jsonpath='{.metadata.uid}')"
echo "    target pod: ${target_pod} (uid=${target_uid})"

echo "==> Step 2: set deterministic inventory error rate to ${BAD_POD_ERROR_RATE}%"
pod_http_post "${target_pod}" "{\"inventoryErrorRate\":${BAD_POD_ERROR_RATE}}"

echo "==> Step 3: wait for ${OUTLIER_EVALUATIONS} outlier evaluations (~${OUTLIER_WAIT_SECONDS}s)"
apply_load_generator 20 10m
sleep "${OUTLIER_WAIT_SECONDS}"

rec_id="$(wait_for_recommendation quarantine_replace)"
echo "==> Step 4: quarantine recommendation"
print_recommendation "${rec_id}"

echo "==> Step 5: pause for operator approval"
pause_or_auto_approve "${rec_id}"

echo "==> Step 6: confirm EndpointSlice drain before deletion"
wait_until_not_routed "${target_pod}"

echo "==> Step 7: confirm replacement becomes ready"
baseline="$(ready_replica_count)"
wait_for_ready_replicas "${baseline}" "${DEMO_WAIT_TIMEOUT}"

echo "==> Step 8: recovered SLI"
wait_for_verification_log "$((DEMO_WAIT_TIMEOUT + 180))"
query_signoz_sli

if kubectl -n "${NAMESPACE}" get pod "${target_pod}" >/dev/null 2>&1; then
  echo "warning: quarantined pod ${target_pod} still exists (replacement may be in progress)" >&2
else
  echo "    quarantined pod ${target_pod} removed"
fi

echo ""
echo "Bad-pod demo complete."
echo "Expected: only ${target_pod} was drained and replaced after approval."
