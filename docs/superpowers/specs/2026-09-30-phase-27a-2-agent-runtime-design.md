# Phase 27a-2: `agent-runtime`, runs and derived agent keys (design)

Status: Draft 2026-09-30, for the owner's review. Program: `2026-09-29-agent-studio-program-design.md` (section 8.3).
Credentials: ADR-033 (Rev 1.1 records 27a-1). Results: ADR-034. Previous sub-phase:
`2026-09-30-phase-27a-1-studio-rules-design.md`.

## 1. Owner decisions (2026-09-30)

- **Who runs an agent:** any live member of the agent's department.
- **The answer:** kept briefly in PostgreSQL for the requester only, then cleared by the sweeper. It is never logged
  or journaled.
- **Scope:** runs, derived keys, the record of each key's master version, rotation at day 60 and the bulk revocation
  all land in 27a-2.

Standing decision (27a split): the runtime reaches EACP through the API only. It has no database connection.

## 2. What 27a-2 delivers

An employee in the agent's department starts a run of an approved Studio agent with its inputs.

1. The new `agent-runtime` binary claims the run under a lease.
2. It derives the agent version's key from its master secret (ADR-033 §2).
3. It executes the steps:
   - each `tool_call` goes through `POST /v1/actions` as the agent, with the employee as the action's subject;
   - it waits for the action, reads earlier outputs through the result channel, and renders the `respond` text.
4. The employee reads the answer for one hour.

The runtime proposes each version's key and its successor at day 60, and a `registry_approver` approves each one.
An operator can revoke every Studio key at once.

Success criteria:

1. A run acts only as the approved `ACTIVE` version it started with, only for its requester, and only through the
   action path. Policy, approvals, budgets and kill scopes apply as to any agent, and PostgreSQL refuses anything
   else. A raw-SQL test shows each refusal.
2. No key and no part of the master secret appears in any response, log line, journal entry, outbox row or run
   record (a canary search, ADR-033 invariant 4).
3. A run fails closed, with a named reason, when its key is missing (`credential_pending`) or expired
   (`credential_expired`), when an action does not succeed, or when its deadline passes. It never resends a step.

## 3. Design

### 3.1 Tables (migration 00028)

Every table follows the RLS convention. `eacp_app` has SELECT only; writes go through the functions in 3.2.

- **`eacp.studio_runs`:**
  - `id`, `agent_id`, `version_id` (the `ACTIVE` version at start), `requested_by` (the principal);
  - `inputs` (jsonb, validated against the definition; cleared when the run ends);
  - `state`: `QUEUED`, `RUNNING`, `SUCCEEDED` or `FAILED`;
  - `failure_reason` (a code), `deadline` (start + `limits.timeout_seconds`);
  - the lease: `runtime_id`, `lease_generation`, `leased_until`;
  - `answer` (text, at most 65 536 bytes, only on `SUCCEEDED`), `answer_expires_at` (finish + 1 hour),
    `answer_pruned_at`;
  - `created_at`, `finished_at`.
  - `zz_audit` journals every change **except** the `inputs` and `answer` columns. The trigger is a variant of
    `audit_row_change` that drops them, as `secret_hash` is dropped today.
- **`eacp.studio_run_steps`:** `run_id`, `step_index`, `step_id`, `action_id` (for a `tool_call`), `recorded_at`.
  - It holds no content; an earlier output is re-read from the result channel when it is needed.
  - It is insert-only and journaled.
- **`eacp.studio_credentials`:** `id` (= `eacp.credentials.id`), `version_id`, `master_version` (`v1`, `v2`…),
  `created_at`.
  - It records the master version behind each derived key (ADR-033 §2).
  - It is insert-only and journaled.

### 3.2 Functions (`SECURITY DEFINER`, reviewed, tenant named in every statement)

The requester is a principal (`storage.SetActor`). The runtime is its own service principal holding
`studio_runtime`, plus a runtime instance id and lease generation for run writes.

| Function | Caller | Rule |
|---|---|---|
| `studio_run_start(agent, inputs jsonb) → uuid` | a live member of the agent's department | the agent has an `ACTIVE`, approved Studio version; inputs are exactly the declared ones, strings within `max_length` |
| `studio_run_claim(runtime_id, limit) → runs` | `studio_runtime` | leases `QUEUED` runs and `RUNNING` runs whose lease lapsed (generation + 1); returns the definition, inputs, version and the requester's subject |
| `studio_run_heartbeat(run, runtime_id, generation)` | the lease holder | extends the lease; a lapsed or stolen lease fails |
| `studio_run_step(run, runtime_id, generation, index, action)` | the lease holder | the action belongs to the run's version, its subject is the requester, its idempotency key is `studio:<run>:<index>`, and the step is a `tool_call` at that index; once per index |
| `studio_run_finish(run, runtime_id, generation, state, answer, reason)` | the lease holder | `SUCCEEDED` needs an answer and every `tool_call` step recorded; `FAILED` needs a reason from a fixed list; clears `inputs` |
| `studio_run_answer(run) → answer` | the requester | only before `answer_expires_at` |
| `studio_runs_expire()` | the sweeper | fails runs past their deadline whose lease lapsed (`deadline_exceeded`); clears expired answers |
| `studio_credential_propose(version, id, hash, master_version)` | `studio_runtime` | inserts the `ak` credential (the 27a-1 guard branch decides) and its `studio_credentials` row; expiry is 90 days |
| `studio_credentials_revoke_all(reason) → count` | `operator` | revokes every live credential of Studio versions, and only those, through the credentials guard (ADR-033 invariant 7) |

Failure reasons: `credential_pending`, `credential_expired`, `action_denied`, `action_failed`, `action_unknown`,
`action_cancelled`, `result_unavailable`, `answer_too_large`, `deadline_exceeded` and `version_replaced`.
`version_replaced` is used when the run's version is no longer `ACTIVE` at a step.

### 3.3 `agent-runtime` (new binary, `cmd/agent-runtime`, package `internal/runtime`)

- **Configuration:**
  - `EACP_API_URL`;
  - `EACP_RUNTIME_KEY_FILE`, the runtime's own `pk` key;
  - `EACP_STUDIO_MASTER_FILE`, at least 32 bytes;
  - `EACP_STUDIO_MASTER_VERSION`, default `v1`;
  - `EACP_RUNTIME_ID`.
  - No database URL. It refuses to start without a master secret, with a shorter one, or with any connector
    secret or provider key configured (ADR-033 invariant 5).
- **Keys:**
  - `identity.KeyFromSecret(kind, tenant, credential, secret)` builds a key from a given secret, and
    `secret = HMAC-SHA-256(master, "eacp-studio-ak-" + version + tenant + credential)`.
  - `identity.Authenticate` is unchanged.
  - Every derived key and the master are added to `logging.SecretSet` before use. Keys are held in memory only.
- **The run loop:**
  - claim, then heartbeat every lease/3;
  - for each step in order:
    - substitute placeholders;
    - `POST /v1/actions` with `Idempotency-Key: studio:<run>:<index>`, the requester's subject and the step's
      fields;
    - record the step;
    - wait on `GET /v1/actions/{id}?wait=`;
    - on `SUCCEEDED`, read `GET /v1/actions/{id}/result` when a later step needs it;
  - render `respond`, then finish.
  - After a crash the next claim resumes at the first unrecorded step. A recorded step is waited on, never resent.
    An unrecorded one is resubmitted with the same idempotency key, so the action API returns the same action.
- **Placeholders:**
  - A payload string that is exactly one placeholder becomes the referenced JSON value.
  - Otherwise each placeholder is replaced by its value, a string as-is and anything else as canonical JSON.
  - A missing path fails the run (`result_unavailable`).
- **Rotation:**
  - A loop lists Studio versions that are `ACTIVE` and approved (`GET /v1/studio/runtime/credentials`).
  - For each one with no approved, unexpired key under the current master version that has more than 30 days left,
    and no pending proposal, it proposes one with a new random credential id.
  - Nothing is ever approved by the runtime.
- **Health:** `/healthz` and `/readyz` with the ADR-029 drain.

### 3.4 API routes

| Route | Who |
|---|---|
| `POST /v1/studio/agents/{id}/runs` (inputs) | a department member (PostgreSQL decides) |
| `GET /v1/studio/runs/{id}` | the requester (with the answer while it lasts); `registry_approver`, `operator` and `auditor` see everything except inputs and the answer |
| `POST /v1/studio/runtime/claims`, `POST /v1/studio/runtime/runs/{id}/heartbeat`, `/steps`, `/finish`, `GET /v1/studio/runtime/credentials`, `POST /v1/studio/runtime/credentials` | `studio_runtime` only |
| `POST /v1/studio/credentials/revoke-all` (reason) | `operator` |

`GET /v1/studio/requests` also lists pending runtime key proposals, with the agent, version, digest and tools, so
one queue shows both (ADR-033 §3.3).

### 3.5 Expiry and incidents (ADR-033 §5)

- An approved key without a successor at day 83 (7 days left) raises a `studio_credential` incident through
  `eacp.incident_evaluate()`, one per version and expiry.
- At expiry the runtime fails runs with `credential_expired`. A key never extends itself.

### 3.6 Unchanged

The action path, the worker, the PDP, approvals, budgets and kill scopes are unchanged, and so are the other
services. A Studio action is an ordinary action of an ordinary agent version.

## 4. Tests (failing first; raw SQL as `eacp_app` for every rule)

- **Runs:**
  - a non-member, a missing or extra input, or an over-long input is refused;
  - an unapproved or non-`ACTIVE` agent is refused;
  - only the lease holder at its generation heartbeats, records or finishes;
  - `studio_run_step` refuses another version's action, another subject, a wrong idempotency key or a second record;
  - `SUCCEEDED` needs every step;
  - the answer is read only by the requester and only before expiry;
  - the sweeper expires deadlines and answers once, with two sweepers racing (ADR-029).
- **Journal:** run changes are journaled without inputs or answer (a canary search).
- **Credentials:**
  - the runtime proposes only for approved versions and records the master version;
  - the bulk revocation revokes all Studio keys and no other key.
- **Runtime (Go, against a real API and database in tests):**
  - the template run end to end (MCP `get_leave_balance` on `mcptest`, the result channel on);
  - a crash between submission and record resumes without a second action;
  - `credential_pending` and `credential_expired`;
  - a denied or approval-waiting action;
  - a replaced version;
  - start refused without, or with a short, master secret or with connector secrets;
  - the canary search for keys and the master in logs, responses, the journal, the outbox and run rows.
- **Catalogue, isolation and invariants:** the new tables and definers in `rls_catalog_test.go`; the isolation flow
  touches them; the tests join invariants 8, 11, 17 and 19.

## 5. Out of scope for 27a-2

The page, the template's registration, the demo and Helm (27a-3). Also out: cancelling a run (the `run` kill scope is
Phase 28), `llm` and `branch` steps, schedules, and a second runtime identity per tenant.

## 6. Open points (conservative defaults)

1. **The answer lives one hour.** It is fixed in PostgreSQL; a later revision may make it a definition limit.
2. **A step whose action ends `UNKNOWN_OUTCOME` fails the run (`action_unknown`).** The run is not resumed after a
   human resolution. A later revision may wait for the resolution.
3. **Outputs are re-read from the result channel.** A template's tools must keep results
   (`result_retention_seconds`) for at least the run's timeout, or later steps fail with `result_unavailable`.
4. **The runtime serves every tenant it has a key for.** It uses one `pk` key per tenant, listed in the key file.
