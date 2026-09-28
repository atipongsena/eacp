#!/usr/bin/env bash
# Example 03: Governance-as-Code. A YAML bundle is validated, planned into a
# change set, submitted by a registry editor and applied only when a
# registry approver approves it. Submitting runs the submit stage (creates
# and proposals); only the approval activates anything. Then drift shows the
# registry in sync with the bundle.
# Requires Go and jq.
set -euo pipefail
cd "$(dirname "$0")"
. ../env.sh
JQ=${JQ:-jq}
root=$(cd ../.. && pwd)
eacpctl() { (cd "$root" && go run ./cmd/eacpctl "$@"); }
export EACP_API_URL=$EACP_API

step() { printf '\n== %s\n' "$*"; }
dir=$(pwd)/bundle

step "1. Validate the bundle (offline: eacpctl resolves the YAML, the target and its variables)"
EACP_API_KEY=$EDITOR_KEY eacpctl bundle validate -C "$dir" |
	"$JQ" -r '"bundle \(.bundle), target \(.target): valid"'

step "2. Plan: what would change (a dry run records nothing)"
plan=$(EACP_API_KEY=$EDITOR_KEY eacpctl bundle plan -C "$dir" --dry-run)
"$JQ" -r 'if (.steps | length) == 0 then "  nothing to change" else .steps[] | "  \(.stage | . + " " * (8 - length)) \(.op) \(.address)" end' <<<"$plan"

step "3. Erin (registry editor) deploys: the submit stage runs, nothing is active yet"
# deploy prints the planned change set, then the submitted one; or a line
# saying there is nothing to change when the registry already matches.
deployed=$(EACP_API_KEY=$EDITOR_KEY eacpctl bundle deploy -C "$dir")
if grep -q '^no changes' <<<"$deployed"; then
	echo "no changes: the registry already matches the bundle (this example ran before)"
else
	submitted=$("$JQ" -s '.[-1]' <<<"$deployed")
	id=$("$JQ" -r .id <<<"$submitted")
	echo "change set $id is $("$JQ" -r .state <<<"$submitted")"

	step "4. Rita (registry approver, a second person) approves: the change set is applied"
	applied=$(EACP_API_KEY=$REGISTRY_APPROVER_KEY eacpctl bundle approve "$id")
	echo "change set $id is $("$JQ" -r .state <<<"$applied")"
	[ "$("$JQ" -r .state <<<"$applied")" = APPLIED ] || { echo "the change set was not applied" >&2; exit 1; }
fi

step "5. Drift: the registry matches the bundle's last applied change set"
drift=$(EACP_API_KEY=$EDITOR_KEY eacpctl bundle drift -C "$dir")
"$JQ" -r '.entries[] | "  \(.status) \(.address)"' <<<"$drift"
[ "$("$JQ" '[.entries[] | select(.status != "in_sync")] | length' <<<"$drift")" = 0 ] ||
	{ echo "the registry drifted from the bundle" >&2; exit 1; }

printf '\nExample 03 passed.\n'
