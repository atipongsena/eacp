#!/usr/bin/env bash
# Regenerates docs/images/console-*.png and studio-*.png from a running
# stack: the examples' data, plus the situations an operator console exists
# for (an approval waiting, an outcome only a human can settle, a kill switch
# and the incidents they open) and an Agent Studio agent from its template to
# an answer. Needs Docker, Go, curl, jq and Chrome or Edge.
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

# Agent Studio (Phase 27a-3b). The runtime gets a fresh random master, written
# once through stdin into its volume (never printed, never in the repository),
# and its own key from examples/.env; then stella's leave-balance agent is
# approved by rita, the runtime proposes its key, rita approves that, and
# stella runs it. A second agent is left waiting for the approver's queue.
studio_file() { # studio_file NAME [keep]: stdin into the runtime's volume, readable by the runtime only
	MSYS_NO_PATHCONV=1 docker run --rm -i -v eacp_studio_runtime:/v busybox:1.37 sh -c \
		"if [ -n '${2:-}' ] && [ -s /v/$1 ]; then cat >/dev/null; else cat > /v/$1; fi; chown 65532:65532 /v/$1; chmod 0400 /v/$1"
}
docker compose --profile studio create agent-runtime >/dev/null
"$python" -c 'import secrets, sys; sys.stdout.write(secrets.token_hex(32))' | studio_file master keep
printf '%s\n' "$STUDIO_RUNTIME_KEY" | studio_file keys
docker compose --profile studio up -d agent-runtime >/dev/null
echo "screenshots: agent-runtime is running"

studio_agent() { # studio_agent NAME prints the agent's id, or nothing
	api "$STUDIO_AUTHOR_KEY" GET /v1/studio/agents | "$JQ" -r --arg n "$1" '.agents[] | select(.name == $n) | .id'
}
save_agent() { # save_agent NAME DISPLAY_NAME prints the new agent's id
	api "$STUDIO_AUTHOR_KEY" POST /v1/studio/agents "$("$JQ" -cn --arg n "$1" --arg d "$2" --arg g "$HR_GROUP_ID" \
		--slurpfile def test/demo/testdata/leave-balance.json '{name: $n, display_name: $d,
		description: "Tells an employee how many days of leave they have left.", department_id: $g,
		definition: $def[0]}')" | "$JQ" -r .agent_id
}
agent=$(studio_agent leave-bot)
[ -n "$agent" ] || agent=$(save_agent leave-bot "Leave balance")
latest() { api "$STUDIO_AUTHOR_KEY" GET /v1/studio/agents | "$JQ" -c --arg a "$agent" '.agents[] | select(.id == $a) | .latest'; }
if [ "$(latest | "$JQ" -r .status)" = waiting_for_approval ]; then
	api "$REGISTRY_APPROVER_KEY" POST "/v1/studio/versions/$(latest | "$JQ" -r .id)/approve" \
		'{"reason": "reads leave balances only"}' >/dev/null
fi
# The runtime proposes the key within EACP_RUNTIME_ROTATE_INTERVAL (a minute).
for _ in $(seq 1 180); do
	status=$(latest | "$JQ" -r .status)
	[ "$status" = ready ] && break
	for key in $(api "$REGISTRY_APPROVER_KEY" GET /v1/studio/requests |
		"$JQ" -r --arg v "$(latest | "$JQ" -r .id)" '.keys[] | select(.version_id == $v and .proposed_by_runtime) | .id'); do
		api "$REGISTRY_APPROVER_KEY" POST "/v1/credentials/$key/approve" >/dev/null
	done
	sleep 1
done
[ "$status" = ready ] || { echo "leave-bot is $status, not ready" >&2; exit 1; }
echo "screenshots: leave-bot is ready"
studio_run=$(api "$STUDIO_AUTHOR_KEY" POST "/v1/studio/agents/$agent/runs" '{"inputs": {"employee_id": "E-1"}}' | "$JQ" -r .id)
for _ in $(seq 1 120); do
	state=$(api "$STUDIO_AUTHOR_KEY" GET "/v1/studio/runs/$studio_run" | "$JQ" -r .state)
	[ "$state" = SUCCEEDED ] || [ "$state" = FAILED ] && break
	sleep 1
done
[ "$state" = SUCCEEDED ] || { echo "run $studio_run is $state" >&2; exit 1; }
echo "screenshots: a leave-balance run answered"
[ -n "$(studio_agent team-leave)" ] || save_agent team-leave "Team leave overview" >/dev/null
echo "screenshots: a second agent waits for a registry approver"

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
(cd tools/screenshots && go run . -page studio -api "$EACP_API" -out "$out" -chrome "$chrome" 	"new:STUDIO_AUTHOR_KEY:#/new?template=leave-balance" 	"agent:STUDIO_AUTHOR_KEY:#/agents/$agent" 	"run:STUDIO_AUTHOR_KEY:#/runs/$studio_run" 	"requests:REGISTRY_APPROVER_KEY:#/requests")
