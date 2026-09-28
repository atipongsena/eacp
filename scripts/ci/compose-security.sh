#!/usr/bin/env bash
# Nightly: the full compose stack and its network-isolation tests.
set -euo pipefail
cd "$(dirname "$0")/../.."

docker compose up -d --build --wait
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/
