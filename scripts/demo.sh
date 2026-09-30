#!/usr/bin/env bash
# Slice A, Slice C, A2A delegation, JIT credential, LLM gateway and Agent Studio demos (MASTER_PLAN §111, docs/DEMO.md).
#
# Starts a fresh, isolated demo stack (compose project "eacp-demo", its own
# volumes, API on 127.0.0.1:18080, PostgreSQL on 127.0.0.1:55433), runs the
# demo scripts in test/demo against it, and removes the demo stack and its
# volumes afterwards. A development stack (project "eacp") is not touched.
# Each demo has its own tenant, so both run on the same stack.
#
#   scripts/demo.sh           run every demo
#   DEMO=A scripts/demo.sh    run some demos: letters from A (Slice A), C (Slice C), D (A2A delegation),
#                             J (JIT credentials), L (the LLM gateway), S (Agent Studio)
#   KEEP=1 scripts/demo.sh    leave the demo stack running afterwards (LLM gateway on 127.0.0.1:18083)
set -euo pipefail

cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1 # Git Bash on Windows: do not rewrite container paths

demos=${DEMO:-ACDJLS}
case "$demos" in
"" | *[!ACDJLS]*)
	echo "DEMO must be letters from A, C, D, J, L and S, or unset" >&2
	exit 2
	;;
esac
tests=()
case "$demos" in *A*) tests+=(TestSliceADemo) ;; esac
case "$demos" in *C*) tests+=(TestSliceCDemo) ;; esac
case "$demos" in *D*) tests+=(TestA2ADemo) ;; esac
case "$demos" in *J*) tests+=(TestJITDemo TestPrivateKeyJWTDemo TestVaultDemo) ;; esac
case "$demos" in *L*) tests+=(TestLLMGatewayDemo) ;; esac
case "$demos" in *S*) tests+=(TestStudioDemo) ;; esac
pattern="^($(
	IFS='|'
	echo "${tests[*]}"
))\$"

compose() {
	docker compose -p eacp-demo -f docker-compose.yml -f deployments/demo/compose.demo.yml "$@"
}

python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py

echo "==> Starting a fresh demo stack (project eacp-demo)"
compose --profile studio down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d --build

status=0
EACP_DEMO=1 go test -count=1 -v -timeout 20m -run "$pattern" ./test/demo || status=$?

if [ "${KEEP:-}" = 1 ]; then
	echo "==> Demo stack left running: API http://127.0.0.1:18080"
else
	compose --profile studio down -v --remove-orphans
fi
exit "$status"
