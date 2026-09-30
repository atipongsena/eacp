# Phase 27a-2 agent-runtime Implementation Plan

> **For agentic workers:** executed inline (native) at the owner's request ("write the implementation plan and dev
> until finished"). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Department members run approved Studio agents. A new `agent-runtime` claims each run and executes it
through the action API with a derived agent key, and the requester reads the answer for one hour. The runtime
proposes keys and their successors, and an operator can revoke every Studio key at once.

**Architecture:**
- Migration 00028 adds the run, step and credential-record tables, the `SECURITY DEFINER` functions that are their
  only writers, a redacting journal trigger and a `studio_credential` incident signal.
- `internal/studio` gains the run and runtime store.
- `internal/api` gains the routes.
- `internal/studioruntime` is the runtime, a client of the HTTP API only, and `cmd/agent-runtime` starts it.

**Tech Stack:** Go 1.26, pgx v5, PostgreSQL 18, `crypto/hmac`.

**Spec:** `docs/superpowers/specs/2026-09-30-phase-27a-2-agent-runtime-design.md`; ADR-033 (Rev 1.1), ADR-034.

## Global Constraints

- Every rule in PostgreSQL, tested with raw SQL as `eacp_app`: 42501 roles and lease holders, 55000 state, 23514
  input rules, 23503 unknown objects.
- `eacp_app` has SELECT on the new tables except `studio_runs.inputs` and `studio_runs.answer`, and no write
  privilege.
- Every definer pins `search_path = pg_catalog, pg_temp`, names the tenant in every statement and is listed in
  `rls_catalog_test.go`.
- Secret derivation: `HMAC-SHA-256(master, "eacp-studio-ak-" + master_version + tenant.String() + credential.String())`,
  32 bytes. Key: `eacp_ak_<tenant hex>_<credential hex>_<base64url(secret)>`.
- The answer lives 3 600 s. Leases are 5–300 s. The step idempotency key is `studio:<run id>:<step index>`.
- Failure reasons: `credential_pending`, `credential_expired`, `action_denied`, `action_failed`,
  `action_unknown`, `action_cancelled`, `result_unavailable`, `answer_too_large`, `deadline_exceeded`,
  `version_replaced`.
- Commit as the user only (no co-author trailer).

## Review Focus

- A runtime crash after `POST /v1/actions` but before `studio_run_step`: the resubmission with the same idempotency
  key returns the same action (test in Task 4).
- A run whose version is replaced mid-run: the next step fails `version_replaced`, and nothing is sent as the new
  version (Task 4).
- Two sweepers expiring the same run or answer: each is counted once (Task 2).
- Two runtimes claiming at once: one lease per run (Task 1, `FOR UPDATE SKIP LOCKED` and generation).
- A placeholder resolving to a missing output key: the run fails `result_unavailable` and never sends the literal
  placeholder (Task 4).

---

### Task 1: Migration 00028 and its raw-SQL tests

**Files:** create `migrations/00028_studio_runs.sql` and `internal/studio/runs_schema_test.go`.

**Produces:**
- Tables `eacp.studio_runs`, `eacp.studio_run_steps` and `eacp.studio_credentials`.
- Functions:
  - `eacp.studio_run_start(uuid, jsonb) → uuid`;
  - `eacp.studio_run_claim(text, text, integer, integer) → SETOF jsonb`, taking the runtime id, master version,
    lease seconds and limit;
  - `eacp.studio_run_heartbeat(uuid, text, bigint, integer)`;
  - `eacp.studio_run_step(uuid, text, bigint, integer, uuid)`;
  - `eacp.studio_run_finish(uuid, text, bigint, text, text, text)`;
  - `eacp.studio_run_answer(uuid) → text`;
  - `eacp.studio_runs_expire(integer) → integer`, run by the sweeper;
  - `eacp.studio_run_tenants() → SETOF uuid`;
  - `eacp.studio_credentials_due(text) → SETOF uuid`;
  - `eacp.studio_credential_propose(uuid, uuid, bytea, text)`;
  - `eacp.studio_credentials_revoke_all(text) → integer`;
  - the trigger `eacp.audit_row_change_redacted()`, which drops the columns named in `TG_ARGV`;
  - an incident kind `studio_credential` in `eacp.incident_evaluate()`.
- Claim returns one JSON object per run: `id`, `version_id`, `agent_id`, `generation`, `deadline`, `definition`,
  `inputs`, `subject`, `steps` (`[{index, action_id}]`), `credential_id` (or null) and `credential` (`ok`, `pending`
  or `expired`).

- [ ] Failing tests:
  - `TestARunIsStartedByADepartmentMemberWithItsInputs`
  - `TestOnlyTheLeaseHolderMovesARun`
  - `TestTwoRuntimesNeverShareARun`
  - `TestAStepIsTheRunsOwnAction`
  - `TestARunSucceedsOnlyWithEveryStepAndAnAnswer`
  - `TestOnlyTheRequesterReadsTheAnswerBeforeItExpires`
  - `TestTheSweeperExpiresRunsAndAnswersOnce`
  - `TestRunsAreJournaledWithoutInputsOrAnswer`
  - `TestTheRuntimeProposesKeysOnlyForApprovedVersionsAndRecordsTheMaster`
  - `TestKeysAreDueBeforeTheyExpire`
  - `TestTheBulkRevocationRevokesOnlyStudioKeys`
  - `TestAnExpiringStudioKeyOpensAnIncident`
- [ ] Implement the migration, with a Down section restoring 00027's objects and 00022's evaluator.

### Task 2: Store, sweeper and API routes

**Files:**
- create `internal/studio/runs.go` and `internal/api/studio_runs_test.go`;
- modify `internal/api/studio.go`, `internal/action/sweeper.go` and `internal/studio/studio.go` (the requests list
  gains key proposals).

**Produces:** on `studio.Service`:
- `Start(ctx, Actor, agent, inputs json.RawMessage) (Run, error)`
- `Run(ctx, Actor, id, reader bool) (Run, error)`
- `Claim(ctx, Actor, ClaimRequest) ([]Claimed, error)`
- `Heartbeat(...)`, `Step(...)` and `Finish(...)`
- `DueCredentials(ctx, Actor, master string) ([]uuid.UUID, error)`
- `Propose(ctx, Actor, Proposal) error`
- `RevokeAll(ctx, Actor, reason) (int, error)`

The action sweeper gains `Stats.StudioExpired`. The routes are those of spec 3.4.

- [ ] Failing tests:
  - `TestStudioRunsThroughTheAPI` (start, runtime claim, step, finish, requester reads the answer, others see no
    answer)
  - `TestRuntimeRoutesAreForTheRuntimeOnly`
  - `TestOperatorsRevokeEveryStudioKey`
  - `TestTheSweeperExpiresStudioRuns`
- [ ] Implement.

### Task 3: Derived keys and runtime configuration

**Files:** modify `internal/identity/apikey.go` and `internal/config/config.go`, with their tests.

**Produces:**
- `identity.KeyFromSecret(kind Kind, tenant, credential uuid.UUID, secret []byte) (key string, hash []byte, err error)`,
  which refuses a secret that is not 32 bytes.
- `config.Options.StudioRuntime` and the `Config` fields `APIURL`, `RuntimeKeyFile`, `StudioMasterFile`,
  `StudioMasterVersion` (`^v[0-9]{1,4}$`, default `v1`), `RuntimeID`, `RuntimeLease` (default 30 s),
  `RuntimeConcurrency` (default 4) and `RuntimePollInterval` (default 1 s).
- Another service refuses `EACP_STUDIO_MASTER_FILE`. The runtime refuses `EACP_DATABASE_URL`, and it inherits the
  existing refusal of connector and provider secrets.

- [ ] Failing tests:
  - `TestKeyFromSecretAuthenticatesUnchanged` (with `identity.Authenticate`)
  - `TestOnlyTheRuntimeHoldsTheStudioMaster`
- [ ] Implement.

### Task 4: `internal/studioruntime` and `cmd/agent-runtime`

**Files:**
- create `internal/studioruntime/{client.go,keys.go,runner.go,render.go,rotate.go}` and their tests;
- create `cmd/agent-runtime/main.go`.

**Produces:**
- `studioruntime.LoadMaster(path, version string) (*Master, error)`, which refuses fewer than 32 bytes.
- `(*Master).Key(tenant, credential uuid.UUID) (string, []byte)`.
- `studioruntime.New(Options{API, Keys map[uuid.UUID]string, Master, ID, Lease, Concurrency, Poll, Log, Redact})`.
- `(*Runtime).RunOnce(ctx) (int, error)` and `(*Runtime).RotateOnce(ctx) (int, error)`.
- `studioruntime.Render(value any, env Env) (any, error)`.

- [ ] Failing tests:
  - `TestRenderSubstitutesInputsAndOutputs` (whole-string placeholders keep the JSON type; a missing path is
    `result_unavailable`)
  - `TestARunEndToEnd`: a real API and a real worker with an in-test connector that returns output, the result
    channel on, and the requester reading the answer
  - `TestACrashBeforeTheStepRecordResubmitsTheSameAction`
  - `TestARunFailsClosedWithoutAKey` (pending and expired)
  - `TestADeniedStepFailsTheRun`
  - `TestAReplacedVersionStopsTheRun`
  - `TestRotationProposesASuccessorAndNeverApproves`
  - `TestNoKeyOrMasterLeaks` (a canary over logs, responses, the journal, the outbox and run rows)
  - `TestTheRuntimeRefusesAWeakMaster`
- [ ] Implement. The runtime is a `service.Main` service with `config.Options{StudioRuntime: true,
  DefaultHTTPAddr: ":8084"}`, runs `RunOnce` and `RotateOnce` loops through `d.Background`, and adds the master and
  every derived key to `d.Redaction()`.

### Task 5: Catalogue, isolation, invariants, docs, full suite

- [ ] `rls_catalog_test.go` (tables and definers).
- [ ] The isolation flow: a run with a step and a finish, and a key proposal.
- [ ] INVARIANTS: runs join 8, 11, 17 and 19.
- [ ] AGENTS.md: the rule, the layout and the status.
- [ ] ADR-033 Rev 1.2, the FEATURES pair, CHANGELOG, MASTER_PLAN, the program spec row and `THIRD_PARTY_NOTICES`
  if needed.
- [ ] `go vet ./... && go test -race -timeout 45m ./...` with `EACP_TEST_ADMIN_DSN`; commit; report and stop.
