#!/usr/bin/env bash
# Regenerates docs/images/console-*.png from a running stack: the examples'
# data, plus the situations an operator console exists for (an approval
# waiting, an outcome only a human can settle, a kill switch and the
# incidents they open). Needs Docker, Go, curl, jq and Chrome or Edge.
# Keys come from examples/.env and are never printed.
set -euo pipefail
cd "$(dirname "$0")/.."
JQ=${JQ:-jq}

# The Fake ERP, MCP, A2A and LLM verifier secrets are generated, git-ignored files.
python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build --wait
bash examples/setup.sh
. examples/env.sh

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
submit() { # submit IDEMPOTENCY_KEY JSON prints the action id
	IDEMPOTENCY_KEY=$1 api "$AGENT_KEY" POST /v1/actions "$2" | "$JQ" -r .id
}
run=$(date +%s)

# The ERP takes the order, times out, and shows it only much later: the
# lookups prove nothing (best effort), so the action waits for a human once
# the reconciler has used its lookups. Submitted first: that takes minutes.
unknown=$(submit "shot-unknown-$run" "$("$JQ" -cn --arg s "$SUBJECT" '{subject: $s, operation: "purchase",
	target: "erp", tool: "erp.create_po_eventual", tool_schema_version: "1", resource: "po",
	payload: {amount: 1200, currency: "THB", supplier: "Contoso Paper",
		scenario: "execute_then_timeout", delay_ms: 5000, visibility_delay_ms: 600000}}')")
echo "screenshots: action $unknown has an unknown outcome"

for example in 01-agent-action 02-llm-gateway 03-governance-as-code; do
	bash "examples/$example/run.sh" >/dev/null
	echo "screenshots: example $example ran"
done

# An approval waiting for amy and ben.
pending=$(submit "shot-approval-$run" "$("$JQ" -cn --arg s "$SUBJECT" '{subject: $s, operation: "purchase_high_value",
	target: "erp", tool: "erp.create_po", tool_schema_version: "1", resource: "po",
	payload: {amount: 480000, currency: "THB", supplier: "Northwind Servers"}}')")
approval=$(api "$OPERATOR_KEY" GET "/v1/actions/$pending" | "$JQ" -r '.approval_request_id // empty')
[ -n "$approval" ] || { echo "action $pending has no approval request" >&2; exit 1; }
echo "screenshots: a purchase is waiting for approval"

# otto stops ledger-bot's active version with a kill switch.
version=$(api "$OPERATOR_KEY" GET /v1/agents/ledger-bot | "$JQ" -r '.versions[] | select(.state == "ACTIVE") | .id')
if [ "$(api "$OPERATOR_KEY" GET /v1/killswitch |
	"$JQ" --arg v "$version" '[.kills[] | select(.target_id == $v and .killed)] | length')" = 0 ]; then
	api "$OPERATOR_KEY" POST /v1/killswitch "$("$JQ" -cn --arg v "$version" '{scope: "agent_version",
		target_id: $v, killed: true, reason_code: "security_incident",
		reason: "ledger-bot posted to an unexpected ledger account"}')" >/dev/null
fi
echo "screenshots: ledger-bot's version is killed"

# Ten lookups (EACP_RECONCILE_MAX_ATTEMPTS) with exponential back-off take
# about four to nine minutes.
for _ in $(seq 1 900); do
	state=$(api "$OPERATOR_KEY" GET "/v1/actions/$unknown" | "$JQ" -r .state)
	[ "$state" = NEEDS_HUMAN_RESOLUTION ] && break
	sleep 1
done
[ "$state" = NEEDS_HUMAN_RESOLUTION ] || { echo "action $unknown is $state, not NEEDS_HUMAN_RESOLUTION" >&2; exit 1; }
echo "screenshots: action $unknown needs a human"

# The incident evaluator runs every EACP_INCIDENT_INTERVAL (15 s by default).
for _ in $(seq 1 60); do
	open=$(api "$OPERATOR_KEY" GET '/v1/incidents?limit=50' |
		"$JQ" '[.incidents[] | select(.kind == "kill" or .kind == "unknown_outcome")] | length')
	[ "$open" -ge 2 ] && break
	sleep 2
done
[ "$open" -ge 2 ] || { echo "the incidents did not open" >&2; exit 1; }
echo "screenshots: incidents are open"

evidence=$(api "$OPERATOR_KEY" GET '/v1/actions?state=SUCCEEDED&limit=50' |
	"$JQ" -r '[.actions[] | select(.operation == "purchase_high_value")][0].id')

chrome=${CHROME:-}
if [ -z "$chrome" ]; then
	for c in google-chrome chromium chrome msedge \
		"/c/Program Files/Google/Chrome/Application/chrome.exe" \
		"/c/Program Files (x86)/Google/Chrome/Application/chrome.exe" \
		"/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" \
		"/c/Program Files/Microsoft/Edge/Application/msedge.exe"; do
		if command -v "$c" >/dev/null 2>&1; then
			chrome=$(command -v "$c")
			break
		fi
	done
fi
[ -n "$chrome" ] || { echo "no Chrome or Edge found; set CHROME" >&2; exit 1; }

out=$(pwd)/docs/images
(cd tools/screenshots && go run . -api "$EACP_API" -out "$out" -chrome "$chrome" -full evidence \
	"approvals:APPROVER_KEY:#/approvals/$approval" \
	"overview:OPERATOR_KEY:#/overview" \
	"incidents:OPERATOR_KEY:#/incidents" \
	"execution:OPERATOR_KEY:#/execution?state=NEEDS_HUMAN_RESOLUTION" \
	"evidence:OPERATOR_KEY:#/execution/$evidence" \
	"fleet:OPERATOR_KEY:#/fleet" \
	"dependencies:OPERATOR_KEY:#/dependencies?kind=tool&id=$TOOL_CREATE_PO_ID" \
	"cost:OPERATOR_KEY:#/cost")
