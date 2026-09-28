#!/usr/bin/env bash
# The load benchmark (MASTER_PLAN §104, §105; docs/BENCHMARKS.md).
#
# For each PDP, starts a fresh, isolated bench stack (compose project
# "eacp-bench", its own volumes, API on 127.0.0.1:28080, gateway on :28083,
# PostgreSQL on :55434), runs cmd/eacp-bench against it and removes the stack
# and its volumes afterwards. A development or demo stack is not touched.
# Results go to bench-results/ (git-ignored); render them with
# `go run ./cmd/eacp-bench report ...`.
#
#   scripts/bench.sh                  the full run: local then microsoft-agt, 100-10,000 agents (1-2 hours)
#   scripts/bench.sh --quick          100 agents, two short steps per PDP (a few minutes)
#   PDPS=local scripts/bench.sh       one PDP
#   ERP_DELAY_MS=50 scripts/bench.sh  the Fake ERP waits 50 ms per call
#   KEEP=1 scripts/bench.sh           leave the last stack running afterwards
set -euo pipefail

cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: do not rewrite container paths

quick=()
for arg in "$@"; do
	case "$arg" in
	--quick) quick=(--quick) ;;
	*)
		echo "usage: scripts/bench.sh [--quick]" >&2
		exit 2
		;;
	esac
done
pdps=${PDPS:-local microsoft-agt}
for pdp in $pdps; do
	case "$pdp" in local | microsoft-agt) ;; *)
		echo "PDPS must list local and/or microsoft-agt" >&2
		exit 2
		;;
	esac
done

python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py

exe="bin/eacp-bench$(go env GOEXE)"
go build -o "$exe" ./cmd/eacp-bench

ready() {
	for _ in $(seq 1 180); do
		if curl -fsS -o /dev/null "$1"; then
			return 0
		fi
		sleep 1
	done
	echo "$1 did not become ready" >&2
	return 1
}

status=0
for pdp in $pdps; do
	files=(docker-compose.yml deployments/bench/compose.bench.yml)
	if [ "$pdp" = local ]; then
		files+=(deployments/bench/compose.bench-local.yml)
	fi
	flags=()
	for f in "${files[@]}"; do
		flags+=(-f "$f")
	done
	compose() { docker compose -p eacp-bench "${flags[@]}" "$@"; }

	echo "==> Starting a fresh bench stack (project eacp-bench, PDP $pdp)"
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	compose up -d --build
	ready http://127.0.0.1:28080/readyz
	ready http://127.0.0.1:28083/readyz

	list=$(
		IFS=,
		echo "${files[*]}"
	)
	"$exe" run --pdp "$pdp" "${quick[@]}" --erp-delay-ms "${ERP_DELAY_MS:-0}" --compose-files "$list" || status=$?

	if [ "${KEEP:-}" = 1 ]; then
		echo "==> Bench stack left running: API http://127.0.0.1:28080"
	else
		compose down -v --remove-orphans
	fi
	if [ "$status" != 0 ]; then
		break
	fi
done
exit "$status"
