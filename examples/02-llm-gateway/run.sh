#!/usr/bin/env bash
# Example 02: an LLM call through the EACP gateway with the official Anthropic SDK.
# Creates a virtual environment in examples/02-llm-gateway/.venv on first run.
set -euo pipefail
cd "$(dirname "$0")"
. ../env.sh

python=python3
command -v python3 >/dev/null 2>&1 || python=python
if [ ! -d .venv ]; then
	"$python" -m venv .venv
fi
if [ -x .venv/bin/python ]; then py=.venv/bin/python; else py=.venv/Scripts/python.exe; fi
"$py" -m pip install --quiet --disable-pip-version-check -r requirements.txt
"$py" main.py
