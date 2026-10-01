# ADR-033: Agent Studio and the runtime's agent credentials

Phase 27c Rev 1.4 design accepted by the owner on 2026-10-01; implementation is in progress. The
[full-builder contract](../superpowers/specs/spec-phase-27c-full-builder/SPEC.md) and its design fix schema-v2
forward graphs, exact derived model/tool capabilities, fenced one-use Studio LLM admission, private bounded
typed output, approval-required real LLM preview and compose/Kubernetes proof. Rev 1.4 is not yet delivered.
The existing v1 definitions and distinct-person approval rules remain binding.

Status: Accepted (Rev 1.0, 2026-09-30; the owner asked to start Phase 27a after reviewing it). Rev 1.1 (2026-09-30) records what Phase 27a-1 built; Rev 1.2 (2026-09-30) records what Phase 27a-2 built; Rev 1.3 (2026-09-30) records the Agent Hub, Phase 27b. Scope: Phase 27-0, 27a and 27b.
Related: ADR-001 (the product boundary, agents never hold enterprise credentials), ADR-003 §5 (API keys), ADR-019 (credential custody and providers), ADR-029 (replicas), ADR-031 (the LLM gateway authenticates agent keys), ADR-034 (the result channel); the program spec `docs/superpowers/specs/2026-09-29-agent-studio-program-design.md` sections 2, 5 and 11.

## Context

Agent Studio lets an employee build an agent from a form, and a new binary, `agent-runtime`, runs it (program spec section 5). A Studio agent is an ordinary EACP agent. Each `tool_call` step goes through `POST /v1/actions`, and each `llm` step goes through the LLM gateway. Both authenticate the agent with an `ak` key: `eacp_ak_<tenant>_<credential>_<secret>`, where the database stores only `SHA-256(secret)` (`internal/identity/apikey.go`). So the runtime must hold a valid key for every agent version it runs.

The registry rules for an agent credential are fixed in `eacp.credentials_guard()` (migration 00003), verified for this ADR:

- the proposer must hold `registry_editor` or `registry_approver`, and the version must not be `RETIRED` or `REVOKED`;
- only a `registry_approver` other than the proposer approves it (`assert_distinct(a, proposed_by)`);
- it expires in the future and at most 90 days away;
- its kind, subject, hash, expiry and proposer never change; only an operator or a registry approver revokes it.

Three constraints shape the design:

- **Store no key.** A table of agent keys, even encrypted, is a new store of live credentials that needs its own key anyway.
- **Keep the two-person rule.** No credential may become valid without a second person, including at rotation.
- **Keep the runtime weak.** `registry_editor` is far too broad for a runtime. It may register connectors, tools, contracts, agents, releases, dependency edges and change sets (the `assert_role(a, 'registry_editor'…)` calls in migrations 00003, 00014, 00015, 00019, 00020 and 00023).

This ADR also records the framing of principle 1: Studio is a governed builder on top of the control plane. It is a client of EACP and never an authority, and it does not turn EACP into a competing workflow engine (MASTER_PLAN §114 is about positioning, not about forbidding a builder).

## Decision

### 1. The runtime's own identity: a new role, `studio_runtime`

`agent-runtime` authenticates as a service principal with its own `pk` key. Like any principal key, it is created by an admin and approved by a second admin. The principal holds exactly one role, **`studio_runtime`**, added to the role list. It never holds `registry_editor`, `registry_approver` or any other role, and the role grant guard refuses to combine `studio_runtime` with another role on one principal.

In PostgreSQL, `studio_runtime` may do only these things:

- **propose** an `ak` credential for a version of a **Studio agent** (an agent created by Studio, marked in 27a), and only while that version's capability request is approved;
- **read and advance** runs under a lease, as 27a specifies;
- nothing else in the registry. It cannot approve anything, and it cannot register or change connectors, tools, contracts, agents, allowlists or releases.

`credentials_guard` gains the `studio_runtime` branch. The existing `registry_editor` and `registry_approver` paths are unchanged.

### 2. Agent keys are derived, never stored

For a version it runs, the runtime chooses a random credential id (UUID v4) and derives the secret:

```text
secret = HMAC-SHA-256(master_secret, "eacp-studio-ak-v1" || tenant_id || credential_id)   (32 bytes)
key    = eacp_ak_<tenant hex>_<credential hex>_<base64url(secret)>
hash   = SHA-256(secret)                                                                  (the only value sent)
```

- The label carries a **master version** (`v1`), so a new master can coexist with the old one during a rotation. The version used for each credential is recorded in a Studio table in 27a, not in the key.
- The runtime re-derives a key when it needs it and keeps it only in memory. It never writes, logs, journals or sends a key anywhere but the `Authorization` header of its own calls to `controlplane-api` and `llm-gateway`.
- Every derived key is registered with `logging.SecretSet` before first use.
- `identity` gains a constructor that builds a key from a given secret. Authentication (`identity.Authenticate`) is unchanged, so an API or gateway replica needs no Studio code and no master secret.

### 3. Proposal and approval

1. A version's derived capability becomes an allowlist request. A `registry_approver` who is not the agent's owner approves it (program spec section 4).
2. Only then may the runtime propose a credential for that version, with a 90-day expiry. PostgreSQL refuses a proposal for any other version, for a non-Studio agent, and for a version whose capability request is not approved.
3. A `registry_approver` approves the credential. They must differ from the proposer, which is the runtime's principal, so no human can be the proposer. Studio shows the capability request and the credential proposal side by side in one queue, with the agent, the version, its definition digest and its tools in plain words. The same approver may approve both in one sitting.
4. The version becomes runnable only once it is `ACTIVE` and has an approved, unexpired credential. Until then a run fails closed with `credential_pending`.

### 4. Custody of the master secret

- **Sources:** a file or Vault KV v2, the same kinds of source ADR-019 already uses. The default is a file mounted from a Kubernetes Secret or a compose secret.
- **Requirements:** at least 32 random bytes, read at start. The runtime refuses to start without a master secret, and refuses one shorter than 32 bytes.
- **Placement:** only the `agent-runtime` pod mounts it; the Helm chart gives no other pod the Secret or the Vault role. `controlplane-api`, `execution-worker` and `llm-gateway` never see it, and the runtime holds no connector secret and no provider key (`config.Options` refuses them, as the gateway refuses connector secrets).
- **Handling:** redacted through `logging.SecretSet`, never logged, never in `Values()` of any store, never in a crash dump the runtime writes.
- **Rotating the master:** add `v2` beside `v1`. The runtime proposes new credentials under `v2` for every version, and each needs a human approval as usual. The `v1` credentials are revoked, or expire, after the new ones are approved, and then `v1` is removed.
- **Losing the master:** every Studio credential is proposed again under a new master and approved again (the program spec's open point 1). Nothing else is lost.

### 5. Rotation and expiry of agent credentials

- The runtime proposes a successor credential 30 days before expiry (at day 60 of 90) and raises an alert that an approval is waiting.
- There is **no standing pre-approval**: every rotation is a second person's decision, because pre-approval would weaken the two-person rule (open point 4).
- If nobody approves the successor in time, runs of that version fail closed with `credential_expired` at the step that needed the key, and the incident evaluator opens an incident. An expiring credential never extends itself.

### 6. Revocation and compromise response

- **One credential:** an operator or registry approver revokes it (as today). Authentication refuses it at once, and the runtime stops using it on the next `unauthorized`.
- **All Studio credentials:** in 27a, an operator can revoke every Studio agent credential in one audited operation (a bulk revoke of the credentials of Studio agents). Together with disabling the runtime's principal, this is the response to a suspected runtime or master compromise. The `agent` and `version` kill scopes stop runs in flight.
- Every action and LLM call records the agent version it came from (not the credential id, which is not stored on actions today). Because each Studio version has its own credentials, the journal traces misuse to the version; each run's steps (27a) name the actions they submitted.

### 7. What never happens

- A Studio agent never holds a connector secret or a provider key (ADR-001, ADR-031); it acts only through `POST /v1/actions` and the gateway.
- The runtime never approves a credential, an allowlist, an approval request or a listing.
- A definition never contains a key or a secret (program spec principle 4).
- A key never appears in a response, a log line, the journal, the outbox, a run's recorded step results or the Studio page.

## Threat model

| Threat | Control | Residual risk |
|---|---|---|
| The runtime host is compromised: its `pk` key and the master secret leak | The attacker can act as any **approved** Studio agent version, within its allowlist, policy, approvals, budget and kill scopes. They cannot act as a non-Studio agent, approve anything or change the registry (`studio_runtime` only). Response: bulk-revoke Studio credentials, disable the runtime principal, rotate the master | For the window before detection, every approved Studio agent can be impersonated. The blast radius is the union of their allowlists, which approvals and budgets bound |
| One derived key leaks (for example a crash dump elsewhere) | It is one agent version's key: revoke it; the runtime proposes a successor | As with any agent key |
| A compromised runtime proposes a credential for a version it should not run | PostgreSQL allows proposals only for Studio versions with an approved capability, and a second person approves each | An approver who approves blindly. The queue shows the agent, version and tools in plain words |
| An insider with `registry_editor` proposes a credential for a Studio version to obtain a key | They choose their own secret (bring your own key), so they would hold a valid key for that version once a second person approves it. The same holds today for every agent | Unchanged from today's registry. The approval queue shows the proposer, and a runtime-proposed credential is marked as such |
| The database is read by an attacker | Only `SHA-256(secret)` is stored; keys cannot be recovered | None beyond today |
| An approver is tricked into approving a runtime credential for the wrong version | Studio shows one request per version, with its definition digest and tools; a credential cannot be proposed before the capability is approved | Social engineering of approvers |
| The master secret is lost | Re-propose and re-approve every Studio credential under a new master | Downtime of Studio agents until the approvals are done |
| Rotation is forgotten | The proposal at day 60, an alert, and a fail-closed expiry with an incident | Studio agents stop at expiry if nobody approves |

## Alternatives considered

- **Store generated keys, encrypted, in PostgreSQL.** Rejected: it adds a store of live keys and still needs a key-encryption key, which is the master secret under another name.
- **Delegation instead of keys: the runtime authenticates as itself and acts "on behalf of" a version through a bound, two-person-approved binding.** No per-agent secret exists. But it adds a second way to authenticate as an agent to `controlplane-api` and `llm-gateway` (ADR-003 §5), which widens the core's authority surface. Its blast radius is the same as the master secret's. Kept as a possible later revision if the key approach proves heavy in the gate.
- **Short-lived keys minted per run.** Rejected: each one would need a second person's approval, or an auto-approval that breaks the two-person rule.
- **Give the runtime `registry_editor`**, as the first draft of the program spec did. Rejected: that role changes the registry broadly.

## Invariants (each a failing test first in 27a)

1. `studio_runtime` can propose an `ak` credential only for a Studio agent's version with an approved capability request, and can do nothing else in the registry (raw SQL as `eacp_app`, one refused case per registry write it must not make).
2. No principal holds `studio_runtime` together with another role.
3. A runtime-proposed credential is approved only by a `registry_approver` other than the runtime's principal. The existing `credentials_guard` rules hold unchanged for every other proposer.
4. The derived key authenticates through the unchanged `identity.Authenticate`. No key, and no part of the master secret, appears in any response, log, journal, outbox row, run record or page (a canary search, as in `TestSecretCanaryNeverLeaks`).
5. The runtime refuses to start without a master secret, with one shorter than 32 bytes, or with any connector secret or provider key configured.
6. An unapproved or expired credential makes a run fail closed (`credential_pending`, `credential_expired`), and never makes it continue with another version's key.
7. The bulk revocation revokes every Studio credential and only those.

## Unresolved assumptions (for the owner's review)

| Assumption | Conservative choice |
|---|---|
| The runtime's privilege | A new single-purpose role `studio_runtime`, never `registry_editor` (a correction to program spec section 5.1) |
| Master secret custody | A file or Vault KV v2, mounted only in the runtime pod; at least 32 bytes; no default value |
| Rotation of agent credentials | Human approval every time, proposed at day 60; fail closed with an incident at expiry |
| Rotation of the master | A versioned label (`v1`, `v2`) and re-approval of every credential under the new version |
| Who approves a runtime credential | Any `registry_approver` other than the proposer, typically the approver of the capability request, in one queue |
| Revoking everything at once | An operator's bulk revocation of Studio credentials, audited, added in 27a |
| Delegation instead of keys | Not now: it widens authentication in the core |
| Where the master version of a credential is recorded | A Studio table in 27a, never in the key and never in `eacp.credentials` |
| Recording the credential id on each action | Not added: the agent version is recorded and each Studio version has its own credentials. A later revision may add it if tracing needs it |

## Out of scope

The Studio data model, the builder, the runtime's run loop and the Hub (Phase 27a and after), SSO for employees, the `run` kill scope (Phase 28, ADR-016 Rev 1.1: every Studio action is bound to its run, and a run fails `killed`), and delegation-based authentication.

## Revision 1.1: what Phase 27a-1 built

Phase 27a-1 (spec `docs/superpowers/specs/2026-09-30-phase-27a-1-studio-rules-design.md`) adds the roles, the Studio data and the credential branch. Migration 00027:

- **Roles.** `studio_author` is a human role. `studio_runtime` is held only by a service principal and only alone: the role guard serialises a principal's grants and refuses the combination in either order, counting pending grants (invariant 2 above). Governance-as-Code bundles accept both roles under the same rules.
- **Studio data.** `eacp.studio_agents` marks an agent as a Studio agent (its department and description); `eacp.studio_versions` holds a version's definition, digest, derived capability and one-time decision. Both key on the registry row's `id` (the spec's `agent_id` and `version_id`), because the journal trigger names every row by `id`. The application role only reads them.
- **Saving.** `eacp.studio_save` (`SECURITY DEFINER`) validates the definition, then creates the agent, the `REGISTERED` version and its allowlist as the author. The registry guards accept a `studio_author` only while the function runs. It proves this with a row in `eacp.studio_save_marks` for the current transaction, opened and closed by the function. The application role can read that table but never write it, and transaction ids never repeat.
- **Narrower than the spec, for safety:**
  - no registry editor can add a version or an allowlist to a Studio agent;
  - a Studio version gets its allowlist and becomes `ACTIVE` only after an approved decision, including after a later containment and release;
  - so no Studio version enters a release before approval, and `eacp.studio_decide` refuses approval while the agent has an open release.
- **Deciding.** `eacp.studio_decide` records the decision first, then retires the agent's `ACTIVE` version, activates the allowlist and moves the version to `ACTIVE` through the unchanged guards (or retires it on rejection). An author deciding their own request is a conflict (`55000`, HTTP 409), as the spec says.
- **The credential branch** (section 1): a `studio_runtime` principal proposes an `ak` credential only for a Studio version with an approved decision and an active allowlist. Every other proposer's path is unchanged, and approval still needs a `registry_approver` other than the proposer (invariants 1 and 3).

The record of the master version behind each derived credential, derived keys, rotation and the bulk revocation are Phase 27a-2.

## Revision 1.2: what Phase 27a-2 built

Phase 27a-2 (spec `docs/superpowers/specs/2026-09-30-phase-27a-2-agent-runtime-design.md`) adds `agent-runtime`, runs and derived keys. Migration 00028 and `cmd/agent-runtime`:

- **The derivation, exactly.** The label is the ASCII text `eacp-studio-ak-`, the master version (`v1`), then the tenant and credential ids in their canonical lower-case hyphenated form, with no separators: `HMAC-SHA-256(master, "eacp-studio-ak-" + version + tenant + credential)`. `identity.KeyFromSecret` builds the key from those 32 bytes and refuses any other length; `identity.Authenticate` is unchanged (invariant 4).
- **Custody.** Only `agent-runtime` accepts `EACP_STUDIO_MASTER_FILE`; every other service refuses it, and the runtime refuses `EACP_DATABASE_URL`, connector secrets and provider keys (invariant 5). The master's source in 27a-2 is a file; Vault KV v2 stays open for a later revision. The runtime reaches EACP only through the API, with one `pk` key per tenant (`EACP_RUNTIME_KEY_FILE`).
- **Keys.** `eacp.studio_credentials` records the master version of each derived key. `eacp.studio_credentials_due` lists `ACTIVE`, approved Studio versions with no pending proposal and no approved key with more than 30 days left under the current master; the runtime proposes a new random credential id for each, sending only the hash, and a `registry_approver` approves it in the Studio queue beside the capability requests. The runtime never approves.
- **Runs.** A live member of the agent's department starts a run (`eacp.studio_run_start`, inputs validated against the definition). The runtime leases runs by runtime id and generation, and only the lease holder heartbeats, records a step or finishes. Each `tool_call` is an ordinary `POST /v1/actions` as the run's version, with the requester as subject and the idempotency key `studio:<run>:<index>`; `eacp.studio_run_step` accepts only that action. After a crash, a recorded step is waited on and an unrecorded one is resubmitted under the same key, so the action API returns the same action.
- **Failing closed.** A run fails with a named reason: `credential_pending` or `credential_expired` at claim (invariant 6), `credential_revoked` when the agent key is refused mid-run, `version_replaced` when the heartbeat reports that the version is no longer `ACTIVE` (the runtime then sends nothing more), `action_denied`, `action_failed`, `action_cancelled`, `action_unknown`, `result_unavailable`, `answer_too_large`, or `deadline_exceeded` (also from the sweeper).
- **Answers and inputs.** The answer is kept one hour for the requester only (`eacp.studio_run_answer`), then cleared by the sweeper; inputs are cleared when the run ends. Neither is readable by `eacp_app` directly, journaled (`eacp.audit_row_change_redacted`) or logged.
- **Revocation and expiry.** `eacp.studio_credentials_revoke_all`, by an operator with a reason, revokes every live Studio key and no other (invariant 7). The incident evaluator opens one `studio_credential` incident per version and expiry when its last approved key has 7 days or less left.
- **Narrower than the spec:** the heartbeat returns whether the run's version is still `ACTIVE`, and the fixed failure reasons gain `credential_revoked`, so a revoked key is never reported as an expired one.

Packaging (the image, compose and the Helm chart), the Studio page and the template are Phase 27a-3.

## Revision 1.3: the Agent Hub (Phase 27b)

Phase 27b (spec `docs/superpowers/specs/2026-09-30-phase-27b-agent-hub-design.md`) adds the Hub of program spec section
6. The department trial of section 8.4 did not take place; the owner chose the fallback, so the gate is recorded as
not met and each open point keeps its conservative default. Migration 00029:

- **Department leads.** `eacp.group_memberships.lead` is set only when an admin adds the membership, only for a human,
  and never changes afterwards (the application role cannot update the column and the guard refuses a change). A
  bundle never sets it. `/v1/me` shows it with each group.
- **Listings.** An agent has at most one listing (`eacp.studio_listings`): scope `DEPARTMENT` (the agent's own
  department) or `ORG`, state `PUBLISHED`, `DEPRECATED` or `WITHDRAWN`, the published version and sorted tags. It
  changes only through a proposal (`eacp.studio_listing_proposals`), made by the agent's owner holding
  `studio_author` for an `ACTIVE`, approved version, one open per agent, decided once or cancelled by its proposer.
- **Tiered approval.** `eacp.studio_listing_decide` accepts a live lead of the agent's department for `DEPARTMENT` and
  an `admin` or `registry_approver` for `ORG`; the tag `template` needs `ORG` and an admin. The proposer and the
  agent's owner never decide (`55000`, HTTP 409). Approval re-checks that the version is still `ACTIVE` and approved,
  then creates or replaces the listing. `eacp.studio_listing_retire` lets the owner, an admin or the scope's approver
  deprecate or withdraw a listing with a reason; both only narrow, so no second person is needed.
- **Visibility and runs.** A `PUBLISHED` or `DEPRECATED` listing reaches the enabled humans of its audience (live
  members of the department, or the tenant); the owner always sees their own. The Hub reads through
  `eacp.studio_hub_listings()` and `eacp.studio_hub_definition(listing)`, which filter by the caller.
  `eacp.studio_run_start` now accepts the owner, while a live member of the department, or anyone a listing reaches
  when its published version is the agent's `ACTIVE` one; otherwise the run is refused (`42501`, or `55000` for a
  listing whose version was replaced).
- **Clones.** `eacp.studio_clone` copies a `PUBLISHED` listing's definition, for a `studio_author` it reaches, into a
  new agent of their department through the same save (`eacp.studio_save_as`, which `eacp.studio_save` now wraps and
  `eacp_app` cannot execute). The copy is `REGISTERED` with an inactive allowlist, no key and no listing, and records
  `eacp.studio_agents.cloned_from_version`.
- **Narrower than the spec, for safety:** without a listing only the owner runs an agent (27a let every member of the
  department); a department listing is bound to the agent's own department; only the owner proposes; a deprecated
  listing is not cloned; drafts with revisions and bundle-seeded templates are not built.

| Assumption | Conservative choice |
|---|---|
| Who runs an unlisted agent | Its owner only; the department needs a lead's approval |
| Who decides a department listing | A lead of the agent's own department, never an admin or approver in their place |
| Who may withdraw | The owner, an admin or the scope's approver, without a second person (it only narrows) |
| Seeing a listing | Enabled humans only; the runtime and other service principals see none |
| Cloning a deprecated listing | Refused |

## Revision 1.4: the full builder (Phase 27c)

The owner accepted the [Phase 27c contract](../superpowers/specs/spec-phase-27c-full-builder/SPEC.md) on
2026-10-01. Migrations 00031–00034 implement it; full race, compose, security, UI and Helm checks passed. Live Kubernetes verification remains pending.

- **Definitions.** Schema v1 is preserved. Schema v2 is a forward-only graph of at most 20 reachable nodes:
  `tool_call`, `llm`, `branch` and `respond`. Each destination is fixed; every path terminates in `respond`.
  References to earlier outputs must dominate their consumer on every path. PostgreSQL validates the definition,
  resolves tenant-local model/tool ids and derives their exact union, including unchosen paths. The sum of declared
  model output caps cannot exceed the definition's token limit. A stale save is refused against its expected latest
  version; copying the retained draft creates an ordinary unapproved agent.
- **Model nodes.** Non-streaming Anthropic Messages and OpenAI Chat Completions only, with an immutable model,
  instruction, input, output cap and bounded closed JSON output schema. No tool selection, schema network resolution
  or arbitrary code. Studio admission requires a leaf hard budget in the model's price unit. The existing gateway,
  PostgreSQL allowlist, PDP, reservation, settlement and containment remain the authorities.
- **Progress and recovery.** PostgreSQL owns the current node and records a pending tool action before waiting.
  A model node has one intent, bound to the run, node, runtime id and lease generation. The derived agent key carries
  `EACP-Studio-Intent`, `EACP-Studio-Runtime`, `EACP-Studio-Generation` and the requester's `EACP-Subject`; these
  identifiers never reach the provider. Admission consumes the intent and binds one call atomically. An unconsumed
  intent can be re-fenced; a consumed intent is never resent. Recovery waits on the original action/call. A stale fence
  is 403 and a consumed intent 409, without a retry hint; a ledger outage remains 503.
- **Private output.** ADR-031's narrow exception retains only validated Studio JSON, at most 65 536 bytes, atomically
  with successful settlement. Reads require the live runtime lease. Terminal runs and deadlines clear it; operators
  see metadata only. Provider envelopes, prompts, headers and keys are never stored, journaled, logged or messaged.
  Duplicate JSON keys, missing values and wrong types fail closed. A killed or replaced run cannot publish late output;
  spend still settles. Clearing a kill never revives the run or allows another provider request.
- **Branches.** `eq`, `ne`, `lt`, `le`, `gt` and `ge` compare fixed literals or declared input/prior-output references.
  Equality requires matching scalar types; ordered comparisons require numbers. JSON decimals and large integers are
  compared exactly, including browser parsing and serialization. Missing/null/container values fail `branch_invalid`.
- **Tests and previews.** Draft tests are local sample-only traversal. Real model preview requires the owner's approved,
  active version and an approved agent key. Its immutable `preview` mode substitutes private tool samples; PostgreSQL
  refuses every action under that run even if the runtime is compromised. Preview spends the same governed model budget.
- **Builder and packaging.** The form has Basics, Inputs, a connected node graph and selected-node inspector, Test and Review, with leave balance, leave request triage
  and procurement request triage fixtures. Every real write remains confirmed and English/Thai text remains literal.
  The runtime has no database URL, connector secret or provider key. `EACP_RUNTIME_LLM_URL` names only the gateway
  origin. Compose permits API/gateway egress on `agents`; optional Helm `llmGateway.enabled` packages a separate,
  tokenless gateway with a named provider Secret and explicit provider peers. Runtime policies grant only API/gateway
  egress plus DNS. Development-only Kubernetes resources reproduce the same demo and isolation.

The department gate remains not met. Free-position canvas persistence, schedules, chat, arbitrary expressions, inbound A2A and
production deployment remain outside Phase 27c. The approved-version preview restriction is the conservative
resolution of the program's draft real-model test proposal.

## Verification

Rev 1.0 is a decision document; it changes no code. Rev 1.1's rules are tested in raw SQL as `eacp_app` in `internal/studio/schema_test.go`, and the API in `internal/api/studio_test.go`. Rev 1.2's rules are tested in raw SQL in `internal/studio/runs_schema_test.go`, the routes in `internal/api/studio_runs_test.go`, and the runtime against a real API, worker and database in `internal/studioruntime` (invariants 4 to 7: `TestKeyFromSecretAuthenticatesUnchanged`, `TestNoKeyOrMasterLeaks`, `TestOnlyTheRuntimeHoldsTheStudioMaster`, `TestTheRuntimeRefusesAWeakMaster`, `TestARunFailsClosedWithoutAKey`, `TestTheBulkRevocationRevokesOnlyStudioKeys`). Rev 1.3's rules are tested in raw SQL in
`internal/studio/hub_schema_test.go`, the routes in `internal/api/studio_hub_test.go`, the page in
`internal/ui/jstest/studioviews.test.mjs` and the whole path in `TestStudioDemo` (S6).
