#!/usr/bin/env bash
# Nightly: the full compose stack and its network-isolation tests.
set -euo pipefail
cd "$(dirname "$0")/../.."

# The Fake ERP, MCP, A2A and LLM verifier secrets are generated, git-ignored files.
python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build --wait
EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/
