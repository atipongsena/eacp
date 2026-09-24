# ADR-024: Fleet operations over the registry lifecycle

Status: Accepted (Rev 1.0, 2026-09-25). Scope: Slice C Phase 17 (MASTER_PLAN §38, §65–§66 and §91).
Related: ADR-003 §2 (version lifecycle), ADR-004 (T2/T10/T16), ADR-016 (kills), ADR-022 (circuits), ADR-023 (tool quarantine), ADR-015 (blast radius).

## Context

MASTER_PLAN §38 asks operators to treat agents as a fleet: list, filter, inspect, pause, resume, kill, quarantine and roll back. EACP already has the per-version lifecycle of ADR-003 §2:

- containment (suspend, quarantine) is single-person;
- granting capability (activate, resume, release) needs a second person;
- the version guard trigger enforces every pair, role and separation of duties.

It also has scoped kills (ADR-016), connector circuits (ADR-022) and tool quarantine (ADR-023). What's missing is a fleet view and a way to apply one lifecycle change to many agents atomically, with evidence of what the operation touched.

A second lifecycle for "fleet state" would create two sources of truth for whether an agent may act. It would also bypass the SoD rules of ADR-003.

## Decision

1. **Fleet operations are lifecycle transitions, never a new authority.**

   | Operation | Transition (ADR-003 §2) | Effect on actions |
   |---|---|---|
   | `pause` | the agent's `ACTIVE` version → `SUSPENDED` | New submissions are denied (T2). Queued actions are denied at dispatch intent (T16b, `agent_not_active`). Calls already executing continue. |
   | `resume` | `SUSPENDED` → `ACTIVE` | Only the versions a given `pause` suspended that are still `SUSPENDED`. |
   | `quarantine` | every non-terminal, non-quarantined version of the agent → `QUARANTINED` | So no sibling version can be activated or rolled back to without a release. |
   | `release` | `QUARANTINED` → `SUSPENDED` | Only the versions a given `quarantine` quarantined that are still `QUARANTINED`. Nothing becomes `ACTIVE`: a `resume` or `rollback` must follow. |
   | `rollback` | current `ACTIVE` (if any) → `SUSPENDED`, then an older `SUSPENDED` version → `ACTIVE` | One agent per operation. The older version is either named explicitly or is the newest `SUSPENDED` version below the one being replaced. |

   The existing version guard authorizes each transition, as it would for a single-version change:
   - `pause` and `quarantine`: `operator` or `registry_approver`.
   - `resume` and `rollback`: activation by a `registry_approver` who is neither the version's creator nor the author of its active allowlist.
   - `release`: a `registry_approver` who did not quarantine the version.

   A fleet operation therefore can't do anything the actor couldn't do version by version.

2. **Pause is not a hold.** `pause` suspends capability. To hold queued work without denying it, or to interrupt calls in flight, an operator uses a kill (ADR-016). Fleet operations never create or clear kills, and kills never change the lifecycle.

3. **Atomic, all-or-nothing, and recorded in PostgreSQL.**
   - The operation is one transaction. `eacp.fleet_operations` holds the operation (kind, selector, reason, actor), and `eacp.fleet_operation_targets` holds one row per version it changed.
   - Inserting a target row performs the transition. Its trigger:
     - locks the version;
     - records the `from` state PostgreSQL reads;
     - checks the pair against the operation's kind and the rollback rules;
     - updates the version with the reason `fleet <kind> <operation id>: <reason>`, so the version guard and `audit_row_change` apply.
   - Targets may only be added in the transaction, and by the actor, that created the operation.
   - A deferred constraint trigger rejects an operation with no target (a rollback needs exactly one activation). It then appends the `fleet.<kind>` audit event, listing the targets, as the operation's last statement.
   - If any version refuses, nothing changes.
   - Both tables are insert-only tenant rows under forced RLS.
   - An operation changes at most 500 versions. A larger selection must be split, or contained with a tenant kill.
   - Target rows are taken in version-id order, except a rollback: it suspends before it activates, because at most one version may be `ACTIVE`.

4. **Explicit selection.** A selector names agents (ids or slugs), or filters by environment, risk class, owning group or a `connector.tool` on the active allowlist. Selecting every agent needs `all: true`. `resume` and `release` select only by a prior operation. A dry run returns the planned targets and changes nothing. Agents with nothing to change are reported as skipped.

5. **The fleet view observes; it never decides.** `GET /v1/fleet/agents` and `GET /v1/fleet/health` read one tenant snapshot (`InTenantReadTx`). Per agent, the view reports:
   - the active version and versions by state;
   - owner status;
   - active kill scopes that match the active version (tenant, team, agent, agent version, or a connector or tool on its allowlist);
   - allowlisted tools the database would not execute (`eacp.action_capability_denial`);
   - open or disabled circuits on allowlisted connectors;
   - open actions by state, and terminal actions in a time window.

   Health:
   - `contained`: no `ACTIVE` version, or a matching kill.
   - `degraded`: any capability drift, an open circuit, an unresolved `UNKNOWN_OUTCOME` or `NEEDS_HUMAN_RESOLUTION`, or an unknown owner. An unknown owner is a disabled owner principal, or an owner group with no enabled current member.
   - `ok`: none of these.

   Each status carries its reasons. Health is an observation. It never grants, withholds or triggers anything, and `ok` doesn't prove that an agent is safe.

## Not decided here

- **Canary** (several active versions) and **upgrade with evaluation** belong to ADR-018 (Release & Evaluation). ADR-003 still allows at most one `ACTIVE` version, so the fleet view reports no canary count.
- **Deployment.** EACP does not deploy agent code. A rollback changes which version is authorized. Agent credentials are bound to a version (ADR-003 §5), so the runtime must run with the rolled-back version's credential. Actions submitted under the suspended version are denied, not re-attributed.

## Verification

- `internal/fleet/schema_test.go`: every rule in raw SQL as `eacp_app`:
  - kind/pair checks;
  - actor and transaction binding;
  - the empty-operation and rollback rules;
  - role and SoD refusal through the version guard;
  - insert-only tables;
  - audit.
- `internal/fleet/fleet_test.go`:
  - selection, dry run and atomicity;
  - pause → resume, quarantine → release, and rollback;
  - fleet view, health reasons and tenant isolation.
- `internal/api/fleet_test.go` and `cmd/eacpctl/fleet_test.go`: the API and CLI.
- `internal/storage/rls_catalog_test.go`: the new tables and functions are reviewed.
