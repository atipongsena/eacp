# ADR-003: Agent Registry, Identity and Capability Model

- **Status:** Accepted (Rev 1.3). Rev 1.3 (Phase 14) amends §4 for MCP tools under [ADR-023](ADR-023-mcp-registry-and-tool-fingerprint.md): discovered tools, the v2 fingerprint and quarantine. Rev 1.1 followed the Codex ADR review ([2026-09-23-adr003-review.md](../reviews/2026-09-23-adr003-review.md)); Rev 1.2 followed the Codex code review ([2026-09-23-phase2-code-review.md](../reviews/2026-09-23-phase2-code-review.md)).
- **Date:** 2026-09-23
- **Related:** MASTER_PLAN §7–§11, §31–§34, §57, §76 (Phase 2), §103 (inv. 11, 17, 19); ADR-001, ADR-004 (principles 6–7), ADR-005 (§4, §5, §5a), ADR-021
- **Rule applied:** where an assumption is unresolved, choose the option that's most conservative for safety and correctness.

## Context

Phase 2 builds the records everything else depends on:

- Who an agent is.
- Which version of it is running and who owns it.
- Which tools it may call.
- Which connector contract governs each tool.
- Which humans may change any of that.

ADR-004 and ADR-005 impose requirements on these records:

- Immutable, pinned versions of **allowlists** and **connector contracts**, each behind an "active" pointer that is row-locked `FOR SHARE` by guarded transitions.
- **Two-person activation**: the author and the activator must differ.
- Monotonic **revocation**.
- Canonical **group memberships** for separation of duties.
- An auditable reason for every lifecycle transition (§8).

## Decision

### 1. Principals, roles and groups

- A `principal` is a `human` or `service` identity inside a tenant.
- **One human, one principal.** A `human` principal has a canonical `subject` (IdP subject or lowercase email) that's **unique per tenant**. Two-person rules compare principal IDs, so this makes "distinct principal" mean "distinct human".
- **Roles are a fixed set** in Slice A (a DB `CHECK`):

  | Role | Can | Human only |
  |---|---|---|
  | `admin` | Create and disable principals, propose and approve role grants, manage groups, propose and approve principal credentials | yes |
  | `registry_editor` | Register agents, versions, connectors, tools. **Propose** allowlists, contracts and agent credentials. | yes (Slice A) |
  | `registry_approver` | **Activate** allowlists, contracts and agent versions. Approve agent credentials. Revoke contracts. | yes |
  | `operator` | Containment: suspend, quarantine, revoke agent versions; revoke agent credentials | yes |
  | `approver` | Vote on action approvals (Phase 3) | yes |
  | `auditor` | Read the audit journal and verify the chain | no |

- No role implies another, and `admin` isn't a superuser of the others.
- **A `service` principal may hold only `auditor` in Slice A**, and a DB trigger enforces this. Otherwise a human who controls a CI service principal could author as the service and approve as themselves, and the two principal IDs would look like two people (Rev 1.2). Letting CI register artifacts needs a recorded controller for each service principal, which is later work.
- **Role grants are two-person.** Admin A proposes a grant and admin B approves it. The rules:
  - B must differ from A.
  - **Neither A nor B may be the grantee**, so nobody grants or approves a role for themselves.
  - A grant is effective only once it's approved and before it's revoked.
  - Revocation takes one admin (it reduces capability) and is monotonic.
- Creating a principal takes one admin, but it grants nothing: every role grant is two-person.
- Disabling a principal takes one admin and is permanent in Slice A.
- **Eligibility is row-locked.** Every privileged write re-checks the actor's effective role in the same transaction, reading the principal and role-grant rows `FOR SHARE`. Revoking a role or disabling a principal `UPDATE`s those rows, so either one serialises with any in-flight privileged write (ADR-004 principle 7).
- `groups` and `group_memberships` are the canonical membership source for separation of duties (ADR-005 §4). In Slice A only EACP-managed memberships count. An IdP sync can come later.
  - Membership changes need one admin and are audited. A removal closes the row; nothing is ever deleted.
  - **For Phase 3:** approval SoD must use the **union** of the group memberships captured when the approval request was created and those at vote time. Leaving a group just before voting therefore doesn't make you eligible.

### 2. Agents and versions

- An `agent` has a slug, environment (`development`, `staging`, `production`) and risk class (`low`, `medium`, `high`, `critical`), plus an **owner: exactly one principal or one group**.
- **Every agent must have an owner, in every environment.** MASTER_PLAN §9 only requires it for production. Requiring it everywhere is stricter and keeps SoD rules uniform.
- **Agent rows are immutable in Slice A:** owner, environment, risk class and slug. Changing ownership means registering a new agent. That removes "change owner, act, change back" windows.
- **Slugs are canonical:** lowercase `[a-z0-9-]`, unique per tenant (tools are unique per connector), and immutable.
- An `agent_version` holds a number assigned by the DB (the previous highest plus one), runtime, and a code reference.
- **Lifecycle (Slice A subset of §8):**

  | From | To | Who | Extra guard |
  |---|---|---|---|
  | `REGISTERED` | `ACTIVE` | `registry_approver`, **≠ version creator, ≠ author of its active allowlist** | Has an active allowlist. No other `ACTIVE` version of this agent. |
  | `ACTIVE` | `SUSPENDED` | `operator` or `registry_approver` | — |
  | `SUSPENDED` | `ACTIVE` | same as activation | same as activation |
  | `REGISTERED`, `ACTIVE`, `SUSPENDED` | `RETIRED` | `registry_approver` | Terminal |
  | `REGISTERED`, `ACTIVE`, `SUSPENDED` | `QUARANTINED` | `operator` or `registry_approver` | — |
  | `QUARANTINED` | `SUSPENDED` | `registry_approver`, **≠ the principal who quarantined it** | — |
  | any non-terminal | `REVOKED` | `operator` or `registry_approver` | Terminal |

  Every transition needs a non-empty **reason** and writes an audit event.

  A DB trigger enforces this whole table: allowed pairs, roles, the two-person rules, and terminal immutability. An `UPDATE` may change either the lifecycle state or the allowlist pointer, never both at once.
- **Containment is single-person and fast.** Transitions that **reduce** capability (suspend, quarantine, revoke, retire) need one person. Transitions that **grant** capability (activate, resume, release from quarantine) always need a second.
- **At most one `ACTIVE` version per agent** in Slice A, enforced by a partial unique index. To activate a new version, first retire or suspend the old one. Canary with several active versions is later work and needs its own ADR.
- **Only an `ACTIVE` version can act** (ADR-004 T2, T10, T16).

### 3. Capability allowlists (immutable plus a pointer)

- An `agent_allowlist` is an **insert-only** revision for one agent version. It holds a set of **tool IDs**, and the insert trigger validates that each exists in the same tenant. Tools are never deleted, so the references stay valid.
- `agent_versions.active_allowlist_id` is the pointer. Activation is an `UPDATE` on that row by a `registry_approver` who **isn't the allowlist's author**.
- Guarded transitions read the pointer row `FOR SHARE` (ADR-004 principle 7).
- An empty allowlist is valid and grants nothing.
- Once set, the pointer can't be cleared. To remove capability quickly, suspend the version (one person). To narrow it permanently, activate a smaller allowlist (two people).

### 4. Connectors, tools and contracts

- A `connector` has a slug, protocol, endpoint and a **`secret_ref`**. That's a name the execution worker maps to a secret it holds. **No secret value is ever stored in the database or returned by the API** (ADR-001 §3).
  - **Secret refs are tenant-namespaced by construction.** The worker resolves `(action.tenant_id, secret_ref)`, never a bare ref. A tenant therefore can't name another tenant's secret, or a platform secret, whatever string it registers.
  - The worker's secret configuration also binds each secret to the endpoint host it may be sent to. Delivered in Phase 5 (`internal/worker.SecretStore`, ADR-004 Rev 2.3): the worker refuses to resolve a secret for any other `host:port`, or for an endpoint URL with userinfo.
- **Connectors and tools are immutable** (no `UPDATE` grant). Rev 1.3: a tool's only updatable columns are its contract pointer, and, under ADR-023, its current definition (moved only when the scanner records one), `missing_since` and its quarantine fields. A new endpoint means a new connector, which has no contract and so can't execute until it's recertified.
- A `tool` belongs to a connector.
- A `tool_contract` is an **insert-only, versioned** operator declaration (MASTER_PLAN §31). Its fields are validated both in Go and by DB `CHECK` constraints:
  - `side_effects`: a non-empty subset of `READ_ONLY`, `REVERSIBLE_WRITE`, `IRREVERSIBLE_WRITE`, `EXTERNAL_COMMUNICATION`, `FINANCIAL`, `ADMINISTRATIVE`. `READ_ONLY` can't be combined with any other class.
  - `idempotency_mode`: `native` (requires `idempotency_key_field`), `correlation_only` (requires `correlation_field`) or `none`. Field names, when present, may not be blank (Rev 1.2).
  - `reconciliation_lookup`: `by_operation_key` or `none`. `reconciliation_consistency`: `strong`, `eventual` or `none`. `none` if and only if there's no lookup.
  - `proof_standard`: `authoritative`, `best_effort` or `none`. **`authoritative` requires `by_operation_key` lookup with `strong` consistency.** `none` is required when there's no lookup.
  - `no_effect_errors`: the error classes certified as no-effect (ADR-004).
  - `credential_custody`: must equal `worker`, enforced by a `CHECK`.
  - `max_attempts` (at least 1). **Any tool that isn't `READ_ONLY` and has `idempotency_mode = none` must have `max_attempts = 1`** (DB `CHECK`). Even then, a retry is only ever the ADR-004 path, which needs a no-effect proof. An ambiguous outcome goes to `UNKNOWN_OUTCOME`, then `NEEDS_HUMAN_RESOLUTION`, never to an automatic retry.
  - Optional: `timeout_ms`, `concurrency_group`, `max_inflight` and `data_sensitivity` (§31). The scheduler uses them in Slice B.
  - **Fingerprint.** On insert, the DB computes `fingerprint = sha256(protocol, endpoint, secret_ref, connector slug, tool slug)` and stores it. `CheckCapability` recomputes it from the current rows and denies on mismatch (`contract_fingerprint_mismatch`). The rows are immutable, so this is a backstop against owner-level edits for HTTP tools.
  - **Rev 1.3, MCP tools (ADR-023 §3).** An MCP tool's fingerprint (v2) also covers its MCP name and the database-computed digest of its current definition. A server-side change to the definition therefore changes the fingerprint, and the contract stops matching. A tool with no definition yet has no fingerprint. An MCP tool's contract pins the definition its proposer reviewed.
- `tools.active_contract_id` is the pointer. Activation is **two-person**: the activator isn't the contract's author. Once set, the pointer can't be cleared; revoke the contract instead.
- **Revocation** sets `revoked_at`, `revoked_by` and `revoke_reason` **once** and can never be undone (a trigger enforces this). Revocation is single-person (`registry_approver` or `operator`), because it only reduces capability.
- A tool is **executable** only if it isn't quarantined (Rev 1.3, denial `tool_quarantined`, checked after the allowlist), and its active contract exists, isn't revoked, and its fingerprint matches.

### 5. Credentials (authentication to EACP)

- **Agent credentials are bound to one `AgentVersion`**, not to the agent. That way a runtime running v14 code can't borrow v15's allowlist. A new version needs a new credential, which is a rotation cost we accept.
- **Principal credentials** are bound to one principal.
- **Key format:** `eacp_<kind>_<tenant-uuid-hex>_<credential-uuid-hex>_<secret>`
  - `kind` is `ak` (agent) or `pk` (principal).
  - `secret` is 32 random bytes, strict base64url.
  - The tenant ID in the key lets the server set RLS context **before** looking up the credential, so no RLS bypass or global lookup table is needed.
  - Only **SHA-256(secret)** is stored. The secret has 256 bits of entropy, so no slow KDF is needed.
  - Comparison is constant-time.
- **Bring your own key (hash-only registration).** EACP never generates or displays a secret for a credential someone else will use:
  - The key holder runs `eacpctl key generate`, which prints the key (kept by the holder) plus its credential ID and hash.
  - Only the credential ID and hash are registered, so neither the proposer nor the approver learns the secret.
- **Registration is two-person.**
  - Principal keys: an `admin` proposes and a different `admin` approves.
  - Agent keys: a `registry_editor` or `registry_approver` proposes and a different `registry_approver` approves.
  - The approver can't be the principal the credential is for.
  - An unapproved credential never authenticates.
- **Every credential expires:** `expires_at` is required and at most **90 days** after the proposal.
- Revocation needs one person (`admin` for principal keys; `registry_approver` or `operator` for agent keys) and is monotonic.
- **Authentication fails** when:
  - the key is malformed;
  - the tenant or credential isn't found;
  - the hash doesn't match;
  - the credential is unapproved, revoked or expired;
  - (principal keys) the principal is disabled;
  - (agent keys) the version is `RETIRED` or `REVOKED`.
- An agent key for any non-terminal version that isn't `ACTIVE` (`REGISTERED`, `SUSPENDED` or `QUARANTINED`) **still authenticates**, so the runtime can read its own status, but has **no capability**.
- Every authentication failure returns the same generic 401. The log records only a reason code and the credential ID, never the key.
- **Authentication isn't enforcement.** Agents can't bypass EACP because of ADR-001 §3: agents hold no connector credentials, and only workers have a network path to connectors (compose networks, `test/security`). This ADR doesn't weaken either.

### 6. Capability check (used by Phases 4–5)

`CheckCapability(tx, agentVersionID, "connector.tool")` runs **inside the caller's tenant transaction**. It reads the agent version, its active allowlist, the tool and its active contract, all `FOR SHARE`. It returns either:

- the tool ID plus the **contract ID (the version to pin)**, or
- a denial reason: `agent_version_not_active`, `unknown_tool`, `tool_not_in_allowlist`, `no_active_contract`, `contract_revoked` or `contract_fingerprint_mismatch`.

Operator changes to any of those rows take row locks, so a concurrent change serialises with the check.

`CheckCapability` is **necessary but not sufficient.** The release boundary (ADR-005 §5a) and dispatch (ADR-004 T16) also lock the tenant policy pointer and evaluate governance. This check just lets them deny early with a precise reason.

### 7. Audit journal (brought forward from Phase 4)

Registry changes are the first privileged operations, so the journal lands now:

- `audit_events(tenant_id, seq, recorded_at, payload, prev_hash, hash)`
- **Append-only:** the app role has no `UPDATE`, `DELETE` or `TRUNCATE`, and triggers reject all three even for the owner.
- **Hash-chained per tenant:**
  - `hash = SHA-256(prev_hash ‖ tenant_id ‖ seq ‖ recorded_at_µs ‖ payload)`
  - `payload` is the canonical JSON of the event, stored verbatim so it can be re-verified.
  - **The DB assigns every chain field:** `seq`, `recorded_at`, `prev_hash` and `hash`. A `SECURITY DEFINER` trigger does it, so client-supplied values are overwritten. Only that trigger can write `audit_chain_heads`.
  - Appends serialise on a per-tenant `audit_chain_heads` row (`FOR UPDATE`).
- **The database writes the registry's audit events** (Rev 1.2). An `AFTER INSERT OR UPDATE` trigger on every registry table appends one event per changed row, in the same transaction. The event records the actor, the table and operation, the reason column that applies, and the changed columns (from → to). Credential hashes are never recorded.
  - So no code path, whether Go or raw SQL, can change the registry without an audit event, and a rolled-back change leaves no event.
  - Go appends events only for things that aren't row changes, such as `tenant.bootstrapped`.
- `Verify(tenant)` recomputes the chain and checks that it ends **exactly** at the head. Any edit, deletion (of the tail or the head included), reordering or rewinding of the head is detected.
  - It must run in a `REPEATABLE READ` snapshot (`storage.InTenantReadTx`) and refuses otherwise. Under `READ COMMITTED`, an append committing between the two reads would look like tampering.
- **Scope:** the chain proves the journal hasn't been altered since it was written. It doesn't prove the writer was honest. The writer is EACP itself, and a compromised application role could append false events. Stopping that needs external anchoring or signing, which Slice C handles.

### 8. Defence in depth: the DB enforces what Go enforces

Go returns friendly errors. PostgreSQL is the backstop against **bugs in the application**:

- RLS on every table.
- `CHECK` constraints on contracts and enums.
- Triggers for:
  - role and two-person rules on every privileged write, with the actor's grants read `FOR SHARE`;
  - the lifecycle transition table and terminal immutability;
  - contract immutability and monotonic revocation;
  - audit append-only.
- A partial unique index for one `ACTIVE` version per agent.
- **Composite foreign keys** `(tenant_id, id)` on every reference, so no row can point into another tenant even if an ID leaks. Unique keys include `tenant_id`.
- Column-level `UPDATE` grants, so only lifecycle, pointer, approval and revocation columns are writable. Rows are never `DELETE`d.
- A missing actor ID is accepted only from the schema owner, the bootstrap path.
- Services refuse to start as the schema owner, or as any role that can become it (`storage.CheckRoleSafety`), because the owner can disable triggers and RLS.

**Trust boundary (Rev 1.2, stated explicitly).** The tenant context (`app.tenant_id`, ADR-021) and the actor (`app.actor_id`) are transaction settings that **the application role itself sets**.

- **What the triggers stop:** an application **bug**. For example, a missing role check, a stale attribution, or a wrong transition all get rejected.
- **What they don't stop:** an attacker who holds the `eacp_app` **database credentials**. That attacker *is* the application as far as PostgreSQL can tell. They can claim any tenant and any actor, and so can act as two principals.
- **Why authenticating inside the DB wouldn't fix it:** every API key passes through the application, so a compromised application would capture the keys it needs.

That credential is therefore protected like a root credential:

- Only `controlplane-api` and `execution-worker` hold it. Agents never do (network isolation, ADR-001 §3).
- No human uses it interactively.

Two later measures narrow the gap further: external anchoring or signing of the audit chain (Slice C, §7), and separate database roles per service (a later ADR).

## Consequences

**Positive**
- Every Slice A requirement from ADR-004 and ADR-005 about registry facts is implementable with row locks, without `SERIALIZABLE`.
- A single insider can't:
  - grant themselves capability;
  - mint a key for someone else and keep the secret;
  - activate their own allowlist, contract or version;
  - un-revoke anything;
  - rewrite audit history.
- Bugs in the Go layer can't do any of those things either, and can't skip an audit event.
- **Limit:** someone holding the application's database credentials can do all of the above (§8, trust boundary).
- Credentials are useless outside their tenant and version, they expire, and a database leak doesn't reveal usable keys.

**Negative / costs**
- Every capability grant takes two people. That's intentional friction, and it needs at least two eligible humans per tenant for each approving role.
- Credentials per version, plus the 90-day expiry, add rotation work.
- The per-tenant audit chain serialises audited writes within a tenant. That's acceptable for registry-rate changes. Execution-rate events (Phase 4+) may need a sharded chain, to be revisited with measurements (§105).
- Much of the enforcement lives in PL/pgSQL triggers, which are harder to read than Go. Every trigger rule has an integration test that bypasses the Go layer.

## Unresolved assumptions and conservative defaults

| Unresolved | Default chosen |
|---|---|
| Should dev/staging agents need an owner? | Yes, all environments. |
| Can one person author and activate in a small team? | No. Two people are always required. |
| Should suspended agents still authenticate? | Yes, but with no capability. It helps operators and runtimes see their status and grants nothing. |
| Key hashing: SHA-256 or a KDF? | SHA-256, because the secret is 256-bit random. A KDF adds nothing against brute force and costs latency on every request. |
| Who generates keys? | The holder (bring your own key). EACP stores only hashes it was given. |
| Tenant bootstrap | `eacpctl tenant create` connects with the schema-owner DSN, the break-glass trust root. It creates the tenant and **two** human `admin` principals, since two-person grants need two, and registers keys for them from hashes the admins generated themselves. It's an operator-only path; the app role can't insert tenants. |
| Can an admin rotate their own key? | Yes, but only through the normal path: they propose their own hash and **another** admin approves. |
| Credential lifetime | 90 days maximum, for every credential. |
| Should agent rows be updatable? | No. Only lifecycle, pointer, approval and revocation columns are, and only on other tables. |

## Verification

- Unit tests: key format, generation and parsing. Contract validation matrix. Lifecycle transition table (every allowed and every forbidden pair).
- Integration tests with RLS against real Postgres, **issuing raw SQL as the app role** so the Go layer is bypassed:
  - Tenant isolation on every new table. Cross-tenant references rejected by the composite FKs.
  - Self-grant, self-approval, single-admin grant, and a privileged role on a service principal: all rejected.
  - Two-person activation of allowlists, contracts and versions; the SoD for quarantine release.
  - Contract un-revoke rejected. Allowlist and connector `UPDATE` rejected. The retry-safety `CHECK`.
  - Audit `UPDATE`/`DELETE`/`TRUNCATE` rejected. Tampering detected by `Verify`.
  - Every `CheckCapability` denial reason, plus the allowed path returning the contract to pin.
  - Authentication failures: malformed, wrong secret, unapproved, revoked, expired, wrong tenant, retired version, disabled principal.
- HTTP tests: 401 without or with a bad key, 403 for a wrong role, 404 for another tenant's resources.
