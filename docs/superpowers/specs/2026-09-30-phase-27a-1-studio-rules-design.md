# Phase 27a-1: Studio data, rules and the author and approver API (design)

Status: Draft 2026-09-30, for the owner's review. Program: `2026-09-29-agent-studio-program-design.md` (section 8.3, the
thin slice). Credentials: ADR-033. Results: ADR-034.

## 1. The split of 27a (owner, 2026-09-30)

The thin slice is delivered in three sub-phases. Each has its own spec, plan and report:

| Sub-phase | Content |
|---|---|
| **27a-1** (this spec) | Roles, the Studio tables, the definition and its derived capability in PostgreSQL, saving versions, the approval path, the runtime's credential branch, the author and approver API |
| 27a-2 | `agent-runtime`: runs under a lease through runtime API routes (no database), derived credentials (ADR-033), `tool_call` and `respond` steps, the result channel, rotation, the bulk revocation |
| 27a-3 | The `/studio/` form and status page, the template (MCP `get_leave_balance` on the fake MCP server), the demo, the Helm deployment |

Owner decisions: a new role `studio_author`; the runtime reaches EACP through the API only; the template uses an MCP
tool.

## 2. What 27a-1 delivers

An employee with `studio_author` saves an agent definition. PostgreSQL validates it, computes its digest and the tools
it needs, and creates:

- a registry agent that the employee owns;
- a `REGISTERED` version;
- an allowlist that holds exactly those tools.

A `registry_approver` who is not the employee sees the request in plain words and approves or rejects it. Approval
activates the version (retiring the previous one) through the existing registry triggers. The employee can always see
where the agent is. The runtime's credential branch (ADR-033) is added and tested, but nothing uses it until 27a-2.

Success criteria:

1. The employee cannot give the agent a tool its steps do not name, approve their own request, or write any
   registry row outside a Studio save. PostgreSQL refuses each attempt, and a raw-SQL test shows it.
2. A definition containing a value that looks like a secret is refused.
3. The status endpoint names the stage and, when waiting, the role that must act.

## 3. Design

### 3.1 Roles

The migration adds two roles to the `role_grants` check:

- **`studio_author`:** may save Studio agents and versions, and nothing else in the registry. Granted like any role
  (admin proposes, a second admin approves).
- **`studio_runtime`** (ADR-033 §1): may propose an `ak` credential for a Studio version whose capability is approved,
  and later (27a-2) use the runtime routes.
  - It is **exclusive**: the role guard refuses to grant it to a principal who holds any other live role, and refuses
    any other role to a principal who holds it.

### 3.2 The definition (schema version 1)

One JSON document, canonicalized (RFC 8785) by the API before it reaches PostgreSQL. PostgreSQL computes its SHA-256
over the text it stores:

```json
{"schema_version": 1, "kind": "agent",
 "inputs": {"employee_id": {"type": "string", "max_length": 64}},
 "steps": [
   {"id": "lookup", "kind": "tool_call", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1",
    "operation": "lookup", "target": "hr", "resource": "leave_balance",
    "payload": {"employee_id": "{{inputs.employee_id}}"}},
   {"id": "answer", "kind": "respond",
    "text": "You have {{steps.lookup.output.structuredContent.days}} days of leave left."}],
 "limits": {"timeout_seconds": 300}}
```

PostgreSQL enforces every rule, with SQLSTATE 23514 and a message naming the rule:

- `schema_version` is exactly 1 (a newer one is refused, never guessed), and `kind` is `agent`.
- **Inputs:** at most 10, named `^[a-z][a-z0-9_]{0,31}$`, each `{"type": "string", "max_length": 1..1024}` (strings
  only in 27a).
- **Steps:** 1 to 20, ids `^[a-z][a-z0-9_]{0,31}$` and unique; kinds `tool_call` and `respond` only. The last step,
  and only the last, is `respond`.
- **`tool_call`:** `tool` names a tool of the tenant as `connector.tool`; `operation`, `target`, `resource` and
  `tool_schema_version` are non-empty strings of at most 128 characters; `payload` is an object.
- **`respond`:** `text` is a string of at most 4 096 characters.
- **Placeholders:** `{{inputs.<declared input>}}` and `{{steps.<earlier step id>.output<.key>*}}` only, where a key
  is `[A-Za-z0-9_]+` and at most 8 deep. A placeholder may appear inside a `payload` string value or in `text`,
  nowhere else. Nothing is evaluated: no code and no expressions.
- `limits.timeout_seconds` is between 10 and 3 600.
- **No secrets:** any string matching a secret pattern is refused (`definition_contains_secret`). The patterns are
  EACP keys (`eacp_ak_`, `eacp_pk_`), `-----BEGIN`, `AKIA` plus 16 characters, `Bearer `, `ghp_`, `xox?-`, and
  `sk-` plus 20 or more characters. This is best effort; the rule it serves is that a definition never needs a secret.
- The whole document is at most 64 KiB.

**Derived capability:** the sorted, distinct `tool` values of the `tool_call` steps, computed by PostgreSQL. It is the
version's allowlist, and nothing else can be added.

### 3.3 Tables (migration 00027)

Every table follows the RLS convention and gets the `zz_audit` trigger (a definition holds no secret, by the rule
above):

- **`eacp.studio_agents`:** `agent_id` (PK with the tenant; references `eacp.agents`), `department_group_id`,
  `description` (at most 1 000 characters), `created_by`, `created_at`. It marks an agent as a Studio agent.
- **`eacp.studio_versions`:** `version_id` (PK with the tenant; references `eacp.agent_versions`), `agent_id`,
  `definition` (text, `IS JSON`, at most 65 536 bytes), `digest` (hex, computed), `capability` (`text[]`, computed),
  `created_by`, `created_at`, `decision` (`approved` or `rejected`, set once), `decided_by`, `decided_at` and
  `decision_reason`. All immutable except the one-time decision.

The application role has no direct INSERT, UPDATE or DELETE on either table. Writes go through the functions below.

### 3.4 Saving (`eacp.studio_save`)

`eacp.studio_save(p_agent uuid, p_name text, p_display_name text, p_description text, p_department uuid,
p_definition text) RETURNS uuid` (the version id), `SECURITY DEFINER`. The actor is the authenticated principal
(`storage.SetActor`), who must hold `studio_author`.

- With `p_agent` NULL it creates the agent:
  - `environment 'production'`, `risk_class 'high'` (the conservative fixed values of 27a);
  - `owner_principal_id` is the author, and the author must be a live member of `p_department`.
- With `p_agent` set, only the agent's Studio owner may add a version, and the agent must be a Studio agent.
- It validates the definition (3.2), then inserts, as the author, in this order:
  1. the agent;
  2. the `studio_agents` row;
  3. the `REGISTERED` version (`runtime 'studio'`, `code_ref 'studio:<digest>'`);
  4. the `studio_versions` row;
  5. the allowlist, holding exactly the capability.
- The registry guards gain a Studio branch. `studio_author` may insert an agent, a version or an allowlist **only
  inside `eacp.studio_save`**. The function proves this through a private, transaction-scoped marker that `eacp_app`
  can neither write nor read directly (`eacp.in_studio_save()`, `SECURITY DEFINER`, reviewed). A `studio_author`
  gets no other registry write.

The author is the version's creator and the allowlist's author, so the existing guards already require the approver
to be someone else.

### 3.5 Approving and rejecting

- **`eacp.studio_decide(p_version uuid, p_approve boolean, p_reason text)`, as a `registry_approver`**, in one
  transaction, through the existing guards:
  1. Record the decision once (a reason is required).
  2. On approval:
     - retire the agent's current `ACTIVE` version, if any, with reason `replaced by version N`;
     - set the allowlist as the version's active one;
     - move the version to `ACTIVE`.
  3. On rejection: `RETIRED` with the reason.
- **The existing guards still decide.** The approver must differ from the version creator and the allowlist author
  (both the owner), and hold `registry_approver`. A version with an open release is refused (the release guard).
- A request that is already decided is refused (`55000`).

### 3.6 The runtime's credential branch (ADR-033 §1 and §3)

- `credentials_guard` gains a branch: an `ak` credential may be proposed by a `studio_runtime` principal only for the
  version of a Studio agent with an approved decision and an active allowlist, with the usual expiry rules. The
  existing paths are unchanged.
- The approval rule is unchanged: a `registry_approver` other than the proposer.

### 3.7 The API (`controlplane-api`)

| Route | Who | What |
|---|---|---|
| `POST /v1/studio/agents` | `studio_author` | name, display name, description, department, definition: the new agent and its version 1 |
| `POST /v1/studio/agents/{id}/versions` | the agent's Studio owner | a definition: the next version |
| `GET /v1/studio/agents` | `studio_author` (own agents), `registry_approver`, `auditor` (all) | agents with their latest version's status |
| `GET /v1/studio/versions/{id}` | the owner, `registry_approver`, `auditor` | definition, digest, capability, status |
| `GET /v1/studio/requests` | `registry_approver` | undecided versions, with each tool's connector, protocol and contract side effects in plain words |
| `POST /v1/studio/versions/{id}/approve` and `/reject` | `registry_approver` | the decision, with a reason |

**Status** is computed from the rows, never stored. Each stage says who acts next:

| Stage | Meaning | Who acts next |
|---|---|---|
| `waiting_for_approval` | Undecided | a `registry_approver` other than the owner |
| `waiting_for_credential` | Approved and `ACTIVE`, without an approved, unexpired credential | the runtime proposes, then a `registry_approver` approves |
| `ready` | Approved and `ACTIVE`, with an approved, unexpired credential | |
| `rejected` | Rejected, with the reason | |
| `replaced` | Retired after a newer version was approved | |
| `suspended`, `quarantined`, `retired` | Other lifecycle states | |

Errors keep the registry's mapping:

- 403 when the role is wrong;
- 404 for another author's agent;
- 400 for an invalid definition, with the rule's message;
- 409 for a decided request or the owner approving.

### 3.8 Unchanged

The Action API, the worker, the PDP, approvals, budgets and kill scopes see a Studio agent as an ordinary agent. 27a-1
adds no binary and no page.

## 4. Tests (failing first; raw SQL as `eacp_app` for every rule)

- **Roles:** both roles are grantable by the two-person path; `studio_runtime` is refused beside any other role, in
  both orders.
- **Saving:**
  - a principal without `studio_author` is refused;
  - an author outside the department is refused;
  - a non-owner adding a version is refused;
  - a direct insert of an agent, version, allowlist, `studio_agents` or `studio_versions` row by a `studio_author`
    outside `eacp.studio_save` is refused;
  - the allowlist equals the derived capability.
- **Definition matrix:** one refused case per rule in 3.2, and the accepted example. The digest equals SHA-256 of the
  stored text; the capability is sorted and distinct.
- **Approval:**
  - the owner cannot approve their own version;
  - a user without `registry_approver` cannot;
  - approval activates the version and retires the previous `ACTIVE` one in the same transaction;
  - rejection retires it;
  - a second decision is refused;
  - the decision is journaled.
- **Credentials:** `studio_runtime` can propose only for an approved Studio version, and not for a non-Studio or
  undecided version; it cannot approve; `registry_editor` paths are unchanged.
- **Catalogue:** the new tables, their owner scans (if any) and the `SECURITY DEFINER` functions in
  `rls_catalog_test.go`; the isolation flow touches the new tables.
- **API:** every route's status codes; the status stages; the request list's plain-words tools.
- **Invariants:** the approval tests join invariant 19 (an executed action needs an `ACTIVE` version whose allowlist
  includes the tool), the journal test joins 17, and the isolation test joins 8.

## 5. Out of scope for 27a-1

The runtime, runs, derived keys and the bulk revocation (27a-2); the page, the template and Helm (27a-3); the `llm`
and `branch` steps, schedules, the Hub, clones and department leads (after the gate).

## 6. Open points (conservative defaults)

1. **Fixed `risk_class 'high'` and `environment 'production'` for Studio agents.** A later revision may derive the
   risk from the tools.
2. **Studio versions never enter a release (canary).** Approval replaces the active version at once. A version with an
   open release is refused.
3. **Placeholders are resolved by the runtime (27a-2) from its own step records.** 27a-1 only validates them.
4. **The secret patterns are best effort.** They are listed in the migration and tested case by case.
