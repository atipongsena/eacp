#!/usr/bin/env bash
# Nightly: the three examples against a fresh compose stack, each twice
# (the examples, like their setup, must be safe to run again).
set -euo pipefail
cd "$(dirname "$0")/../.."

# The Fake ERP, MCP, A2A and LLM verifier secrets are generated, git-ignored files.
python=python3
command -v python3 >/dev/null 2>&1 || python=python
"$python" deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d --build --wait
bash examples/setup.sh
bash examples/setup.sh
for example in 01-agent-action 02-llm-gateway 03-governance-as-code; do
	bash "examples/$example/run.sh"
	bash "examples/$example/run.sh"
done
