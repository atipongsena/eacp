#!/usr/bin/env bash
# Example 01: an agent's high-value purchase order goes through EACP:
# governance, two approvals, release, execution against the Fake ERP,
# and the evidence of all of it. Requires curl and jq.
set -euo pipefail
cd "$(dirname "$0")"
. ../env.sh
JQ=${JQ:-jq}

step() { printf '\n== %s\n' "$*"; }
# api KEY METHOD PATH [JSON] prints the response body; it fails on a non-2xx status.
api() {
	local key=$1 method=$2 path=$3 body=${4:-}
	local args=(-sS -X "$method" -H "Authorization: Bearer $key" -w '\n%{http_code}')
	[ -n "$body" ] && args+=(-H 'Content-Type: application/json' --data "$body")
	[ -n "${IDEMPOTENCY_KEY:-}" ] && args+=(-H "Idempotency-Key: $IDEMPOTENCY_KEY")
	local out code
	out=$(curl "${args[@]}" "$EACP_API$path")
	code=${out##*$'\n'}
	out=${out%$'\n'*}
	if [ "${code:0:1}" != 2 ]; then
		echo "$method $path: HTTP $code $out" >&2
		return 1
	fi
	printf '%s' "$out"
}

order="po-$(date +%s)"
body=$("$JQ" -cn --arg subject "$SUBJECT" --arg order "$order" '{
	subject: $subject, operation: "purchase_high_value", target: "erp", tool: "erp.create_po",
	tool_schema_version: "1", resource: "po",
	payload: {amount: 250000, currency: "THB", supplier: "ACME Office Supply", order: $order}}')

step "1. The agent submits a 250,000 THB purchase order"
action=$(IDEMPOTENCY_KEY=$order api "$AGENT_KEY" POST '/v1/actions?wait=2s' "$body")
id=$("$JQ" -r .id <<<"$action")
request=$("$JQ" -r .approval_request_id <<<"$action")
echo "action $id is $("$JQ" -r .state <<<"$action"): the policy escalates high-value purchases to two approvers"

step "2. The agent retries the same request: EACP answers with the same action"
again=$(IDEMPOTENCY_KEY=$order api "$AGENT_KEY" POST '/v1/actions?wait=1s' "$body")
[ "$("$JQ" -r .id <<<"$again")" = "$id" ] || { echo "a retry created another action" >&2; exit 1; }
echo "same idempotency key → action $id again, nothing new is created"

step "3. Two approvers vote"
vote='{"decision": "APPROVE", "reason": "within budget and the supplier is approved"}'
echo "amy:  request $("$JQ" -r .request_state <<<"$(api "$APPROVER_KEY" POST "/v1/approvals/$request/votes" "$vote")")"
echo "ben:  request $("$JQ" -r .request_state <<<"$(api "$APPROVER2_KEY" POST "/v1/approvals/$request/votes" "$vote")")"

step "4. EACP releases the action and the worker executes it against the Fake ERP"
state=""
for _ in $(seq 1 120); do
	now=$("$JQ" -r .state <<<"$(api "$AGENT_KEY" GET "/v1/actions/$id")")
	[ "$now" != "$state" ] && echo "state: $now" && state=$now
	case "$state" in SUCCEEDED) break ;; FAILED | DENIED | CANCELLED | EXPIRED | NEEDS_HUMAN_RESOLUTION)
		echo "the action ended $state" >&2; exit 1 ;;
	esac
	sleep 0.5
done
[ "$state" = SUCCEEDED ] || { echo "the action did not finish in time" >&2; exit 1; }

step "5. The evidence, as an operator sees it"
evidence=$(api "$OPERATOR_KEY" GET "/v1/actions/$id/evidence")
"$JQ" -r '
	(.decisions[] | "governance: \(.verdict) under policy v\(.policy_version) (\(.reasons | join(", ")))"),
	(.approvals[] | "approval: \(.state), quorum \(.required_quorum), \(.votes | length) votes, grant consumed by this action: \(.grant.consumed_by_action_id != null)"),
	(.attempts[] | "attempt \(.attempt): \(.outcome), external reference \(.external_reference)"),
	"journal: \([.journal[] | select(.data.to != null) | .data.to] | join(" → "))",
	"audit chain: \(.chain.count) entries, verified: \(.chain.verified)"' <<<"$evidence"
[ "$("$JQ" -r .chain.verified <<<"$evidence")" = true ] || { echo "the audit chain does not verify" >&2; exit 1; }

printf '\nExample 01 passed.\n'
