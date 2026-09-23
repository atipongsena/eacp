# Phase 2 Code Review (Codex cross-review)

- **Date:** 2026-09-23
- **Scope:** Slice A / Phase 2, Registry, Identity and Capability. Reviewed files:
  - migrations `00002` and `00003`;
  - `internal/{storage,audit,identity,registry,api}`;
  - `cmd/eacpctl` bootstrap and client;
  - the schema tests;
  - ADR-003 Rev 1.1.
- **Reviewer:** Codex (`gpt-5.5`, read-only, files inlined). Adjudicated by Claude Code.
- **Codex verdict before fixes:** "not fit to commit"
- **Outcome:**
  - Finding 1 is **accepted as a documentation defect**: ADR-003 overclaimed what the database protects against, and now states the trust boundary.
  - Findings 2–5 are **accepted and fixed** test-first.
  - One more issue, found by Claude Code while Codex was reviewing, is fixed.
  - ADR-003 is now **Rev 1.2**.

| # | Sev. | Finding | Decision | Fix | Regression test |
|---|---|---|---|---|---|
| 1 | Critical | `app.tenant_id` / `app.actor_id` are settings the app role controls, so direct SQL as `eacp_app` can claim any tenant or actor and act as two principals | **Accepted as a documentation defect; no code change** | The database role *is* the application. Anyone holding its credentials is inside the trust boundary, the same as for tenant context since Phase 1 (ADR-021). Authenticating inside the DB wouldn't help, because every key passes through the application. ADR-003 §8 claimed protection against "direct SQL"; it now states the boundary (the triggers stop application **bugs**, not a stolen application credential) and how that credential is protected. Slice C adds audit anchoring and per-service roles. | — |
| 2 | High | Audit events are written only by Go; a raw-SQL change isn't audited | **Accepted** | `eacp.audit_row_change()`, an `AFTER INSERT OR UPDATE` trigger on all 11 registry tables. It appends one event per changed row (actor, action, reason, changed columns), in the same transaction, and never records `secret_hash`. Go no longer appends registry events. | `TestRawSQLChangesAreAuditedByTheDatabase`, `TestAuditNeverRecordsCredentialHashes` |
| 3 | High | A human who controls a service principal holding `registry_editor` can author as the service and approve as themselves | **Accepted (most conservative option)** | In Slice A, service principals may hold only `auditor`. | `TestServicePrincipalsHoldOnlyAuditor` |
| 4 | High | A blank `idempotency_key_field` / `correlation_field` satisfies `native` / `correlation_only`, which bypasses the retry-safety backstop | **Accepted** | `CHECK field_names_not_blank` | `TestContractFieldNamesCannotBeBlank` |
| 5 | Medium | `Verify` under `READ COMMITTED` can report a false `ErrChainBroken` during concurrent appends | **Accepted** | `Verify` requires a `REPEATABLE READ` snapshot (`storage.InTenantReadTx`) and returns `ErrNeedsSnapshot` otherwise. It checks that the chain ends **exactly** at the head, which also detects a deleted or rewound head. | `TestVerifyIsStableUnderConcurrentAppends`, `TestVerifyRefusesReadCommitted`, `TestVerifyDetectsTampering/{deleted head,rewound head}` |
| C1 | High | *(found by Claude Code)* `CheckRoleSafety` accepted the **schema owner** and members of that role. Those roles can `ALTER TABLE … NO FORCE ROW LEVEL SECURITY` or disable the registry triggers. | **Fixed** | The role check also rejects the owner of schema `eacp` and any role that is a member of it | `TestRoleSafetyRejectsSchemaOwnerAndItsMembers` |

## Mutation checks

Each rule below was deleted or weakened one at a time, and a named test failed every time (scratchpad script `mutate.py`):

- grant approver ≠ proposer;
- service-principal roles;
- the DB audit trigger;
- stripping `secret_hash` from audit events;
- non-blank field names;
- version activator ≠ allowlist author;
- quarantine-release SoD;
- contract two-person activation;
- no activation of a revoked contract;
- `unsafe_writes_single_attempt`;
- the effective-role filter on revoked grants;
- credential holder ≠ approver;
- credential lifetime;
- terminal-version freeze.

These were checked the same way:

- removing `FOR SHARE` from `CheckCapability` makes `TestCapabilityCheckSerialisesWithSuspension` fail;
- removing the head comparison from `Verify` makes the tamper test fail.

## Verification (after fixes)

- `gofmt -l .` prints nothing and `go vet ./...` is clean.
- `go test -race ./...` passes with `EACP_TEST_ADMIN_DSN` set.
- `EACP_COMPOSE_TEST=1 go test ./test/security/` passes against the rebuilt stack.
- An end-to-end smoke run passes against the running containers:
  - `eacpctl key generate` and `tenant create`;
  - principals, grants and bring-your-own-key credentials through the API;
  - connector → tool → contract → agent → version → allowlist → activation;
  - agent key: `capability-check` is allowed, unknown tools are denied, and a suspension removes the capability at once;
  - `audit/verify` returns valid.
