#!/usr/bin/env bash
# CI tests with the race detector against the compose PostgreSQL.
#   scripts/ci/test.sh slow   internal/registry and internal/worker (the long ones)
#   scripts/ci/test.sh rest   every other package
set -euo pipefail
cd "$(dirname "$0")/../.."

shard=${1:?usage: scripts/ci/test.sh slow|rest}
case "$shard" in
slow) packages=$(go list ./internal/registry/... ./internal/worker/...) ;;
rest) packages=$(go list ./... | grep -v -e /internal/registry -e /internal/worker) ;;
*) echo "unknown shard $shard (slow|rest)" >&2; exit 2 ;;
esac

# The Fake ERP, MCP, A2A and LLM verifier secrets are generated, git-ignored files.
python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --wait postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
# Without node the console's JavaScript tests would skip; in CI they must run.
export EACP_UI_NODE_REQUIRED=1
# shellcheck disable=SC2086
go test -race -count=1 -timeout 45m $packages
