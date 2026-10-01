# Phase 29 inbound A2A implementation plan

Status: all four tasks complete; [verification evidence](../specs/spec-phase-29-inbound-a2a/VERIFICATION.md).

> Execute inline using Superpowers executing-plans and TDD. The owner forbids Codex reviewers; do not dispatch reviewers or subagents.

**Spec:** `docs/superpowers/specs/spec-phase-29-inbound-a2a/SPEC.md`.
**Baseline:** `3cc6dbe`; keep owner changes to `.gitignore` and `.ignore` untouched.
**Architecture:** optional routes on controlplane-api, projecting existing action engine rows; no schema change, production dependency, service or task store. The owner approved reference-client dependencies; final `go mod tidy` retains only an explicit indirect `golang.org/x/mod v0.41.0` requirement from the existing graph, with no checksum change.

## Global constraints

Authorization for auth/env/deployment integration must precede those edits. Preserve all inherited invariants and existing outbound A2A. No content/key/error-detail logging; no downloads without approval. Run real PostgreSQL tests with `-race`. Commit as owner without attribution, never push. Stop after Phase 29.

## Task 1 — protocol and action projection

Files: create `internal/api/a2a_test.go`, `internal/api/a2a.go`; modify `internal/api/api.go`.

Interfaces: `Server.WithA2A(publicURL string)` opts into routes; actions remain `Server.actions`. Pinned SDK is test-only. Strict wire structs use verified A2A 1.0 shapes.

1. Add failing tests for discovery, disabled defaults, agent-only authentication, SendMessage and same-ID replay/conflict, static status projection, ownership and conservative cancellation. Run `go test -race -run A2A ./internal/api`; expect missing option/routes.
2. Implement bounded I-JSON decoding, static card, supported method validation and core calls. Do not return action views or raw errors.
3. Add failing tests for malformed/ambiguous requests, unsupported versions/methods, oversized input, supplied tenant, non-JSON/text/history/push/multi-turn requests, governance outage with existing task and admission without task. Implement only missing guards; rerun narrow tests.
4. Add pinned upstream client interop and private result tests against the actual API/engine/database. Cover cross-agent and cross-tenant denial, revoked/non-active versions, no history/content logging and unknown outcomes. Rerun the whole API package with race.

## Task 2 — opt-in configuration and development packaging

Files: `internal/config/config.go`, config tests, `cmd/controlplane-api/main.go`, `docker-compose.yml`, Helm templates/values, `test/helm`.

Interfaces: blank `EACP_A2A_PUBLIC_URL` disables routes; exact `/a2a` URL; HTTPS outside development/test. No new secrets/listener/egress.

1. Write failing config and chart render tests for defaults, valid opt-in, unsafe URL refusal, explicit endpoint advertised without Host-derived authority, production HTTP refusal and absent secret values.
2. Implement config, API startup wiring and explicit dev endpoints in compose/Helm. Run config/API/chart tests with race and pinned Helm.
3. Record accepted ADR-030 Rev 1.1 and source reference from pinned module; preserve normative outbound sections.

## Task 3 — live demos and documentation

Files: `test/demo/z_inbound_a2a_test.go`, `scripts/demo.sh`, `scripts/k8s-e2e.sh`, development Helm values if needed, paired `docs/DEMO`, `USER_GUIDE`, `FEATURES`, `ARCHITECTURE`, `KUBERNETES`, README where relevant; `docs/INVARIANTS.md`.

Interfaces: existing demo bootstrap and real ERP action/approval/result channel. New `DEMO=I` selector; Kubernetes includes inbound test over API only. No test harness secrets printed.

1. Write/run failing live demo before implementation gaps are filled. Demonstrate reference-client Send/Get, approval, replay producing one effect, private task, denied capability/kill, cancellation and private result.
2. Update scripts and docs in EN/TH with matching headings/code/images. Stage new docs before guards. Record invariant test names.
3. Run compose inbound and existing outbound demos; narrow documentation guards and Helm tests.

## Task 4 — phase verification and owner commit

1. Run lint. Stop other stacks and start only test PostgreSQL; run `go vet ./...` and `EACP_UI_NODE_REQUIRED=1 EACP_HELM_REQUIRED=1 go test -race -timeout 45m ./...` with admin DSN. Resolve failures without weakening tests.
2. Run full compose build/wait, all examples twice, compose security; run live Kubernetes/Calico gate for chart/deployment changes. Screenshots only if UI changes (none planned).
3. Inspect final diff and requirements, write verification evidence and update phase statuses. Fresh docs/invariant guards after final docs changes.
4. Commit scoped files under owner identity; preserve owner untracked changes. Restore preexisting development stack and clean up owned background test resources. Report commit, passing commands, assumptions and any unverified items; stop.

## Verification focus

Transport must never manufacture a no-effect proof after a task exists, expose another agent's task/result, enqueue a second action on changed replay, pretend in-flight cancellation succeeded, leak request bodies/errors/credentials, or introduce a second state machine. No action authority is moved to Go.
