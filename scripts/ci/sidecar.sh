#!/usr/bin/env bash
# CI: the AGT sidecar's suites and the ADR-002 conformance set through the
# pinned AGT/ACS/OPA stack (the Dockerfile's test stage).
set -euo pipefail
cd "$(dirname "$0")/../.."

docker build -f sidecars/agt-pdp/Dockerfile --target test .
