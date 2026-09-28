#!/usr/bin/env bash
# CI: known vulnerabilities reachable from EACP's code (govulncheck).
set -euo pipefail
cd "$(dirname "$0")/../.."

go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
