# Governance-as-Code (bundles, plan, change sets, drift) — design

Date: 2026-09-25. Status: approved in conversation; awaiting written-spec review.
Normative home once implemented: **ADR-026 Governance-as-Code** (new; next free number).
Phases: **Phase 20** (engine + core registry) and **Phase 21** (identity, roles, policy, budgets).
The former Later phases shift by two: Agent SOC becomes Phase 22, Kubernetes/HA 23, JIT credentials 24,
A2A & LLM gateway 25. MASTER_PLAN section numbers do not change; the new phases are §93a and §93b.

## 1. Intent

Registry owners want to declare the governed estate — connectors, tools, contracts, agents, versions,
allowlists and, later, principals, roles, policies and budgets — in files kept in Git, review a plan, and
apply it. This borrows the ideas of Terraform (plan / apply / state / import / drift) and Databricks Asset
Bundles (a bundle directory, targets, variables, `validate` / `deploy`).

Success means:

- A bundle can be planned against a tenant and the plan shows exactly the steps that would run.
- Applying it never grants anything a person could not grant through the existing API, and every
  two-person rule still needs two people.
- A plan computed against one state cannot be applied to another.
- Drift between the last applied bundle and the database is visible without changing anything.

It is **not** a new authority. PostgreSQL triggers stay the only place that decides a registry write
(ADR-003 §8), and the change set only sequences writes that the triggers already allow.

## 2. Concept mapping

| Borrowed concept | EACP meaning | Deliberately not copied |
|---|---|---|
| DAB bundle, targets, variables | A YAML directory: `eacp.yml` plus `resources/*.yml`. A target binds an API URL and a tenant. `eacpctl` resolves `${var.*}` before sending. | Secret interpolation. A bundle never contains a secret; connectors keep `secret_ref`. |
| Terraform plan | The server plans in a tenant read-only snapshot and records a change set, its steps and a PostgreSQL-computed `base_digest`. | A plan that writes anything. |
| Terraform state | **PostgreSQL is the state.** `eacp.bundle_resources` maps addresses (`agent.procurement`) to object ids. | A state file or a lock file; the open change set is the lock. |
| Terraform apply | Two stages: `submit` (person A) and `approve` (person B ≠ A). Each is one transaction bound to one actor. | Single-person apply of anything that is two-person today. |
| Terraform import, DAB bind | An `import` block adopts an existing object into the bundle without changing it. | — |
| Drift detection | A read-only comparison of the last applied desired document with the database. | Automatic remediation. |
| Destroy, force replacement | Nothing is deleted. A managed resource missing from the file is reported as `orphan`; with `prune: true` it becomes an existing lifecycle move only (retire a version, revoke a contract; in Phase 21 revoke a role grant or remove a membership). A change to an immutable field is `unsupported` and blocks the plan. | Deletion and replacement. |
| Upgrading a live agent | Activating a version while another version of the agent is `ACTIVE` is `requires_release` and blocks the plan. Releases stay with ADR-018. | Bypassing canary. |

Credentials are out of scope: they expire within 90 days and use bring-your-own-key hashes, so they are
rotation, not desired state.

## 3. Bundle format (client side)

```yaml
bundle: { name: procurement }
targets:
  prod: { api: https://eacp.example.com, tenant: acme }
variables:
  erp_endpoint: { default: http://fakeerp:8081 }
resources:
  connectors:
    erp:
      protocol: http
      endpoint: ${var.erp_endpoint}
      secret_ref: erp-token
      tools:
        create_po:
          contract: { side_effects: [write], idempotency_mode: key, ... }   # registry.Contract fields
  agents:
    procurement:
      display_name: Procurement agent
      environment: prod
      risk_class: high
      owner: { group: procurement-team }          # or { principal: <name> }
      version: { runtime: python3.12, code_ref: git:abc123 }
      allowlist: [erp.create_po]
      state: ACTIVE                                # REGISTERED (default) or ACTIVE
import:
  - { to: connector.erp, id: 5b0c...-uuid }
prune: false
```

- `eacpctl` reads YAML, merges `resources/*.yml`, applies the chosen target's overrides and variables, and
  sends one resolved JSON document. The server accepts JSON only and rejects any remaining `${`.
- Unknown fields are rejected (strict decoding) on both sides.
- A YAML library is added to `go.mod` for `cmd/eacpctl` only. Its module path and version are checked against
  upstream before it is pinned (MASTER_PLAN §107).
- Addresses are `<kind>.<name>`: `connector.erp`, `tool.erp.create_po`, `contract.erp.create_po`,
  `agent.procurement`, `version.procurement`, `allowlist.procurement`.

## 4. Planner (server side, `internal/bundle`)

`Plan(ctx, actor, bundle, desired, prune)` runs in `storage.InTenantReadTx` (repeatable read, read only):

1. Validate the document (names, enum values, contract fields with the same checks the API uses).
2. Resolve every address: through `eacp.bundle_resources` for managed ones, by natural key for `import`
   targets and references (connector name, tool `connector.tool`, owner principal or group name).
3. Diff each resource and emit ordered steps. Order follows dependencies: connector → tool → contract
   (propose) → agent → version → allowlist (propose) → [approve stage] contract activate → allowlist
   activate → version transition.
4. Emit findings. Blocking findings — `unsupported`, `requires_release`, `unresolved_reference`,
   `unresolved_variable`, `invalid` — make the plan fail with 422 and record nothing. Non-blocking
   findings — `orphan` without prune, `noop` — are returned with the plan.
5. Unless `dry_run`, record the change set (section 5), its steps and its refs (every object the plan read)
   in a separate write transaction. PostgreSQL computes `base_digest` from the refs at insert.

Diff rules for Phase 20:

| Resource | Create | Change | Removed from file |
|---|---|---|---|
| connector | `create` (submit) | any field → `unsupported` (immutable) | `orphan` (never pruned) |
| tool | `create` (submit) | — | `orphan` (never pruned) |
| contract | `propose` (submit) + `activate` (approve) | different from the active contract → new `propose` + `activate` | prune → `revoke` (approve stage) |
| agent | `create` (submit) | any field → `unsupported` (immutable) | `orphan` (never pruned) |
| version | `create` (submit) when no version matches `runtime` + `code_ref` | a different `code_ref` → new version `create` | prune → `transition RETIRED` (approve stage) |
| allowlist | `propose` (submit) + `activate` (approve) | set differs from the active allowlist → new `propose` + `activate` | — |
| version state | `transition ACTIVE` (approve) if no other version of the agent is `ACTIVE` | otherwise `requires_release` (blocking) | — |

For an MCP tool, the contract pins the tool's current `definition_id`, which the planner reads; a
quarantined or missing MCP tool is `unresolved_reference`.

## 5. Data model (migration 00020)

All tables are tenant-scoped with the RLS convention of `00001_foundation.sql`, get `*_by`/`*_at` from
triggers, and are added to `internal/storage/rls_catalog_test.go`.

- `eacp.bundles (tenant_id, id, name UNIQUE per tenant, created_by, created_at)`.
- `eacp.bundle_resources (tenant_id, bundle_id, address, kind, object_id, change_set_id, managed_at)`,
  unique `(bundle_id, address)` and unique `(tenant_id, kind, object_id)`: an object belongs to at most one
  bundle. Rows are inserted only by an executing change set; they are never deleted in Phase 20.
- `eacp.change_sets (tenant_id, id, bundle_id, state, desired jsonb, desired_digest, base_digest, prune,
  planned_by/at, submitted_by/at, submitted_xact xid8, approved_by/at, approved_xact xid8,
  closed_by/at, close_reason)`.
  - `state ∈ PLANNED, SUBMITTED, APPLIED, REJECTED, SUPERSEDED`.
  - Partial unique index on `bundle_id WHERE state IN ('PLANNED','SUBMITTED')`: one open change set per
    bundle. Planning a new one marks an open `PLANNED` one `SUPERSEDED`; an open `SUBMITTED` one blocks with
    `change_set_open` (it holds pending proposals that must be approved or rejected first).
  - `desired_digest` = SHA-256 of `desired::text` (jsonb text is canonical), computed by PostgreSQL.
- `eacp.change_set_steps (tenant_id, change_set_id, ordinal, address, op, stage, payload jsonb,
  object_id, executed_xact xid8)`.
  - `op ∈ create, propose, activate, transition, revoke, import`; `stage ∈ submit, approve`.
  - Steps are inserted only with their change set in `PLANNED`. `object_id`/`executed_xact` can be set only
    once, and only by the transaction that submitted (submit steps) or approved (approve steps).
- `eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)`.
- `eacp.change_set_base_digest(change_set_id)` (SECURITY INVOKER): a digest over the current registry rows
  of every ref and every managed resource of the bundle (ids, immutable fields, states, active allowlist
  and contract ids). It is added to the RLS catalog review.

### Guards (PostgreSQL)

`change_sets_guard` (BEFORE UPDATE) allows only:

| From | To | Actor and checks |
|---|---|---|
| PLANNED | SUBMITTED | a principal holding `registry_editor`, `registry_approver` or `admin`; recomputed `base_digest` must match, else `change_set_stale`. Binds `submitted_by = eacp.actor()`, `submitted_xact = pg_current_xact_id()`. |
| SUBMITTED | APPLIED | a principal **≠ `submitted_by`** (`eacp.assert_distinct`); digest recomputed over the refs, excluding the objects the submit created, must still match; every proposal the submit created must still be pending. Binds `approved_by`, `approved_xact`. |
| PLANNED | APPLIED | only when the change set has no approve-stage steps; same checks as PLANNED → SUBMITTED. |
| PLANNED, SUBMITTED | REJECTED | the submitter, an approver or an admin, with a reason. Pending proposals stay inert, as they do today. |
| PLANNED | SUPERSEDED | only by a new plan for the same bundle in the same transaction. |

`change_set_steps_guard` enforces the single-assignment and same-transaction rules above.

A deferred constraint trigger `change_sets_commit` (as in `fleet_operations_commit`) checks at commit that
every step of the executed stage has an `object_id`, then appends the audit event
`change_set.submitted|applied|rejected` as the last statement.

### Execution (Go)

`Submit` and `Approve` each run in one `registry.Service.change`-style transaction bound with
`storage.SetActor`:

1. Lock the change set row (`FOR UPDATE`), then update its state (the guard checks the actor and digest).
2. Run the stage's steps in ordinal order through the same SQL as `registry.Service` (refactored so that
   the SQL takes a `pgx.Tx`), so every existing trigger — roles, two-person, ≠ creator, ≠ allowlist author,
   MCP definition pin, one-`ACTIVE` release guard — decides each step.
3. Record each step's `object_id`; insert `bundle_resources` rows for created or imported objects.
4. Commit. Any failing step rolls back the whole stage; the change set keeps its previous state and the
   error names the step's address.

## 6. Drift

`GET /v1/bundles/{name}/drift` runs in a read-only snapshot. It compares the `desired` document of the
bundle's last `APPLIED` change set with the current rows and reports each address as `in_sync`,
`modified` (for example the version was suspended or quarantined, or an allowlist or contract was activated
outside the bundle), `missing` (a managed object is retired, revoked or unreadable) or
`unmanaged_reference` (a referenced object is not in any bundle). It never writes and never blocks.

## 7. API and CLI

API (JSON only; roles are checked in Go for routing and again by the triggers):

| Route | Roles |
|---|---|
| `POST /v1/change-sets` `{bundle, desired, prune, dry_run}` → plan | registry_editor, registry_approver, admin |
| `GET /v1/change-sets`, `GET /v1/change-sets/{id}` | the same plus auditor |
| `POST /v1/change-sets/{id}/submit` | registry_editor, registry_approver, admin |
| `POST /v1/change-sets/{id}/approve` | registry_approver, admin (the triggers still check each step) |
| `POST /v1/change-sets/{id}/reject` `{reason}` | registry_editor, registry_approver, admin |
| `GET /v1/bundles`, `GET /v1/bundles/{name}/drift` | registry_editor, registry_approver, admin, auditor |

Errors: 409 `change_set_stale`, 409 `change_set_open`, 403 `same_principal`, 422 with findings for blocking
plans, 422 `step_failed` with the step's address for a trigger rejection.

CLI (`eacpctl bundle`): `validate` (offline), `plan`, `deploy` (plan + submit), `approve <id>`,
`reject <id>`, `drift`, `status`; each takes `-t <target>` and `-C <dir>`.

## 8. Invariants (tested first)

1. A plan never writes a registry object; only change-set tables are written.
2. No registry write happens through a change set unless its `base_digest` matches the current state.
3. The approve stage cannot be executed by the submitter.
4. Every step still passes the trigger that governs the same write through the API.
5. A change set never deletes a row, and prune uses only existing lifecycle moves.
6. A change set never activates a version while another version of the same agent is `ACTIVE`.
7. A stage is atomic: all of its steps commit or none do.
8. The desired document never carries a secret value (only `secret_ref` names).
9. An object belongs to at most one bundle.

Each invariant gets at least one raw-SQL test as `eacp_app` where it is a database rule, and they are
listed in `docs/INVARIANTS.md`.

## 9. Testing

- Schema tests (raw SQL as `eacp_app`): every guard transition and rejection, one open change set per
  bundle, same-principal approve, stale digest at submit and approve, single assignment of step results,
  steps outside the executing transaction, audit at commit, RLS isolation between tenants.
- Planner tests: table-driven golden plans for create, no-op, new version, allowlist change, contract
  change, immutable change, `requires_release`, orphan with and without prune, import, unresolved
  references and variables.
- Executor integration tests: submit by A and approve by B apply everything; approve by A fails with
  nothing applied; a failing step in the middle rolls back the stage; a concurrent registry change makes the
  change set stale; an MCP contract pins the current definition.
- API and CLI tests, including YAML resolution, targets and strict decoding.
- Everything runs with `go vet ./...` and `go test -race ./...` with `EACP_TEST_ADMIN_DSN` set; the demo gains
  a bundle step.

## 10. Phase 21 (outline, designed in detail before it starts)

Adds principals, groups, memberships, role grants (propose → approve; prune → revoke), governance policy
versions (create → activate), budget accounts and limit changes (raise stays two-person; a raise is an
approve-stage step), FinOps soft limits and forward-only price additions. It reuses the Phase 20 engine and
tables; new resource kinds only add planner rules and step kinds.

## 11. Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Whether approving a change set should also be required when it has only single-person steps | No new requirement; it applies at submit. Recorded so a later ADR can tighten it. |
| What happens to pending proposals when a submitted change set is rejected | They stay pending and inert, as proposals do today; they cannot activate without a second person. |
| Scope of `base_digest` | The refs plus every managed resource of the bundle, not the whole tenant, so unrelated changes do not stale a plan. |
| Credentials in bundles | Excluded. |
