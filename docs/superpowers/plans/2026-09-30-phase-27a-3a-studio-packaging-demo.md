# Phase 27a-3a Implementation Plan: the leave-balance tool, packaging and the Studio demo

> **For agentic workers:** executed inline (the owner asked to develop until the phase is finished). Steps use
> checkbox (`- [ ]`) syntax.

**Goal:** run the leave-balance Studio agent for real on the compose stack, and package `agent-runtime` for compose
and Helm.

**Architecture:** a second fakemcp instance serves `get_leave_balance`; `agent-runtime` joins the image, compose
(profile `studio`, `agents` network, a read-only volume for its master and keys) and the chart (off by default); a
new compose demo drives the whole path through the API.

**Tech stack:** Go, docker compose, Helm (pinned in `.tools/`), the existing demo harness.

**Spec:** `docs/superpowers/specs/2026-09-30-phase-27a-3a-studio-packaging-demo-design.md`

## Global constraints

- No change to the API, the schema or `internal/studioruntime`.
- The chart never renders a secret value; the runtime's pod gets no database, connector or provider secret.
- No master or key is committed; the demo generates the master and writes both files into the volume.
- Doc pairs change together (`TestTranslationsMatch`); new documents are `git add`ed before the guards run.

## Review focus

- The Slice C demo keeps its single `get_po` tool (the default list is unchanged).
- A plain `docker compose up` still starts cleanly (the runtime is behind a profile).
- A narrowed `api.ingress.from` still admits the runtime.
- `studio.env` cannot smuggle a database URL or a secret file into the runtime.
- The demo's leak scan includes the runtime's own log.

---

### Task 1: `get_leave_balance` in fakemcp

**Files:** `internal/fakemcp/fakemcp.go`, `internal/fakemcp/fakemcp_test.go`, `deployments/docker/fakemcp/hr-tools.json`.

- [ ] Failing tests: `TestLeaveBalanceAnswersStructuredContent`, `TestLeaveBalanceErrIsAToolError`,
  `TestAToolOutsideTheListIsRefused`, `TestTheHRToolFileNamesTheLeaveTool`.
- [ ] Implement: `call` answers a tool only when the current list names it; `get_leave_balance` per the spec.

### Task 2: image, compose and compose security tests

**Files:** `deployments/docker/Dockerfile`, `docker-compose.yml`, `deployments/docker/secrets/connector-secrets.dev.json`,
`deployments/docker/secrets/prepare_fakeerp_token.py`, `.gitignore`, `scripts/ci/release-binaries.sh`,
`test/opensource/release_test.go`, `test/security/studio_test.go`.

- [ ] Failing tests: `TestTheRuntimeReachesOnlyTheAPI`, `TestOnlyTheRuntimeHoldsTheStudioMaster` (compose),
  `TestFakeMCPHRRejectsUnauthenticatedCalls`; the release list.
- [ ] Implement and run `EACP_COMPOSE_TEST=1 go test ./test/security/` on a fresh stack.

### Task 3: Helm

**Files:** `deployments/helm/eacp/values.yaml`, `templates/runtime.yaml`, `templates/networkpolicy.yaml`,
`templates/serviceaccount.yaml`, `templates/validate.yaml`, `test/helm/studio_test.go`, `docs/KUBERNETES.md`.

- [ ] Failing tests: `TestTheRuntimeIsOffByDefault`, `TestTheRuntimeNeedsItsSecrets`, `TestTheRuntimePodIsHardened`,
  `TestTheRuntimeReachesOnlyTheAPIInTheCluster`, `TestANarrowedAPIIngressStillAdmitsTheRuntime`,
  `TestRuntimeEnvIsValidated`.
- [ ] Implement; `EACP_HELM_REQUIRED=1 go test ./test/helm`.

### Task 4: the Studio demo

**Files:** `test/demo/studio_test.go`, `test/demo/testdata/leave-balance.json`, `test/demo/platform_test.go`,
`scripts/demo.sh`, `docs/DEMO.md`, `docs/DEMO.th.md`.

- [ ] Write the demo (spec 3.3); `DEMO=S scripts/demo.sh` green; then the whole `scripts/demo.sh`.

### Task 5: docs and verification

- [ ] AGENTS.md (status, commands, layout), CHANGELOG, MASTER_PLAN, the program spec row, `docs/KUBERNETES.md`.
- [ ] `go vet ./... && go test -race -timeout 45m ./...`, the Helm tests, the compose security tests, the demos, and
  `scripts/k8s-e2e.sh` if the cluster tools are present; commit; report and stop.
