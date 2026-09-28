#!/usr/bin/env bash
# Prepares the examples tenant on a running stack (docker compose up -d --build --wait)
# and writes its keys to examples/.env. Safe to run again.
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./examples/setup "$@"
