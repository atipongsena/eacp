# Phase 1 Code Review (Codex cross-review)

- **Date:** 2026-09-23
- **Scope:** Slice A / Phase 1 Platform Foundation. All Go, SQL, compose and Docker files.
- **Reviewer:** Codex (`gpt-5.5`, read-only, files inlined). Adjudicated by Claude Code.
- **Codex verdict before fixes:** "close, but not fit to commit"
- **Outcome:** all six findings **accepted and fixed**. Every fix started with a failing test (TDD).

| # | Sev. | Finding | Fix | Regression test |
|---|---|---|---|---|
| 1 | High | `eacpctl migrate down-all` was allowed when `EACP_ENV` was unset | Requires an explicit `EACP_ENV=development\|test` **and** `--yes-destroy-all-data` | `TestDownAllIsRefusedOutsideDevelopment` (includes an empty env), `TestDownAllRequiresExplicitConfirmation` |
| 2 | High | `CheckRoleSafety` inspected only `current_user`, so a `SET ROLE` session or membership in a BYPASSRLS role passed | Checks `session_user` **and** `current_user`, plus membership in any superuser or BYPASSRLS role (`pg_has_role ... 'MEMBER'`) | `TestRoleSafetyRejectsMembershipInBypassRole`, `TestRoleSafetyRejectsSuperuserSessionUsingSetRole` |
| 3 | High | Keyword DSNs (`host=… password=…`) bypassed password redaction | Only `postgres://` or `postgresql://` URL DSNs are accepted | `TestLoadRejectsInvalidValues/keyword_dsn` |
| 4 | Medium | Services started on an unmigrated or older schema and only failed `/readyz` | `service.Start` now fails closed if the schema is older than the binary | `TestStartRefusesUnmigratedSchema` |
| 5 | Medium | `TestAgentCannotReachPostgres` could pass because the `nc` probe was broken | Positive control using the same `nc` invocation against a reachable port | `TestAgentNcProbeWorks` |
| 6 | Low | A readiness check that ignores `ctx` could hang `/readyz` | Results are collected through a channel with a deadline, and a stuck check reports `timeout` | `TestReadinessTimesOutChecksThatIgnoreContext` |

Accepted trade-off: a check that ignores its context keeps running in a goroutine after `/readyz` has answered. Checks are expected to honour `ctx`. This is documented on `health.Check`.

## Verification (after fixes)

- `go vet ./...` is clean and `gofmt -l .` prints nothing.
- `go test -race ./...` passes, **with** `EACP_TEST_ADMIN_DSN` set, so the PostgreSQL integration tests ran rather than skipped.
- `EACP_COMPOSE_TEST=1 go test ./test/security/` passes against a freshly rebuilt stack.
- Mutation checks done during Phase 1:
  - Disabling RLS on `eacp.tenants` makes three isolation tests fail.
  - Attaching the agent container to the erp network makes `TestAgentCannotReachFakeERP` fail.
- Manual: the API container exits 1 when given a superuser DSN, and `docker compose stop` logs "http stopped cleanly" with exit code 0.
