# Governance-as-Code, Phase 21 (identity, policy, budgets, prices) — design

Status: approved in conversation on 2026-09-25; normative detail goes into ADR-026 (Rev 1.1).
Builds on: [the Phase 20 design](2026-09-25-governance-as-code-design.md), ADR-026 Rev 1.0, migration 00020.

## 1. Intent

A tenant's people and money are governance too. Phase 21 lets the same bundle that declares connectors and
agents also declare principals, groups, memberships and role grants; the tenant's governance policy; budget
accounts with their hard and soft limits; and model prices. It reuses the Phase 20 engine unchanged in
shape: one JSON document, a recorded plan, a two-stage two-person apply, a digest that makes a moved
registry stale, and read-only drift.

Success: a pull request that changes `eacp.yml` can grant a role, roll out a policy, raise a budget or add a
price with the same review path as an agent change, and no bundle can do what the API forbids.

Non-goals: credentials in bundles; disabling principals or deleting anything; future-dated or backdated
prices; changing a group's scheduler weight after creation (no API does either).

## 2. Decisions

| Question | Decision |
|---|---|
| Architecture | Approach A: extend `internal/bundle` in place, one planner file per domain. No provider interface (YAGNI). |
| Shared SQL | Each domain exposes a `Tx` like `registry.Tx`: identity methods on `registry.Tx`, new `governance.Tx`, `budget.Tx`, `finops.Tx`. The API services call them, so API and bundle steps run the same SQL and the same triggers. |
| Mixed bundles | Allowed. The change-set roles already admit `admin` (plan/submit: editor, approver or admin; approve: approver or admin). Each step's own trigger decides; a submitter without `admin` fails an identity step (`step_failed`). |
| Admin prune | Prune may revoke admin grants, but never below two approved admins on enabled human principals: a blocking `admin_floor` finding at plan time, and a PostgreSQL check at the stage's commit. |
| Principals | Never disabled by a bundle (disabling is permanent). An undeclared managed principal is an `orphan` finding. |
| Pending proposals | A grant or limit change already open from the API blocks the plan (`pending`): decide it there first. |
| Policy ownership | One bundle per tenant may own `policy`. |

## 3. Bundle format

Client side (eacpctl), YAML:

```yaml
resources:
  principals:
    alice:  {kind: human, subject: alice@acme.com, display_name: Alice, roles: [admin, registry_approver]}
    ci-bot: {kind: service, display_name: CI bot, roles: [auditor]}
  groups:
    procurement: {display_name: Procurement, schedule_weight: 2, members: [alice, bob]}
  policy: {file: policies/tenant.json}
  budgets:
    root:  {unit: USD, hard_limit: 1000}
    buyer: {unit: USD, parent: root, agent: buyer, hard_limit: 200, soft_limit: 150}
  prices:
    - {provider: openai, model: gpt-x, unit: USD, input_per_mtok: 2.5, output_per_mtok: 10}
```

eacpctl reads `policy.file` relative to the bundle directory (it must stay inside it), parses it as JSON and
sends `"policy": {"content": {...}}`. The server never reads files. Variables work in every field as in
Phase 20. Decoding stays strict: an unknown field is refused, so no secret can ride along.

Wire document additions (`bundle.Document`):

- `principals: map[name]{kind, subject?, display_name, roles[]}`: `kind` is `human` or `service`; `name`,
  `kind` and `subject` are immutable (a change is `unsupported`); `roles` is the full set of approved roles.
- `groups: map[name]{display_name, schedule_weight?, members[]}`: `schedule_weight` 1–10, set only at
  creation (a change is `unsupported`); `members` names principals, declared here or existing.
- `policy: {content}`: the local policy bundle, validated with `governance.ValidatePolicy`.
- `budgets: map[name]{unit, parent?, agent?, hard_limit, soft_limit?}`: `unit`, `parent` and `agent` are
  immutable; `parent` names another budget; `agent` names an agent declared in the bundle or existing;
  amounts are decimal numbers; an omitted `soft_limit` leaves the current one alone.
- `prices: [{provider, model, unit, input_per_mtok, cached_input_per_mtok?, output_per_mtok}]`: at most one
  row per provider and model; no `effective_from`.
- `imports` accept the new addresses, so existing principals, groups, budgets and the policy can be adopted.

Validation (blocking `invalid`): names are slugs as in Phase 20; a service principal may hold only
`auditor` (mirrors `role_grants_guard`, which still decides); roles come from the role CHECK list; a
principal named twice in `members`; a budget cycle, a parent in another unit, or a parent bound to an
agent (an agent's account has no children); a duplicate price key;
malformed amounts; policy content that fails validation.

## 4. Addresses and managed objects

| Address | Object | Managed id |
|---|---|---|
| `principal.alice` | principal | principal id |
| `group.procurement` | group | group id |
| `member.procurement.alice` | membership | membership id |
| `role.alice.admin` | role grant | grant id |
| `policy` | tenant policy | the policy version the bundle last activated |
| `budget.buyer` | budget account | account id |
| `price.openai.gpt-x` | model price | the price row the bundle last added |

`bundle_resources` gains these kinds. A partial unique index allows one `policy` row per tenant. Grants and
memberships are managed through their principal and group: a declared principal manages all of its live
grants, a declared group all of its live memberships.

## 5. Planner

The planner reads the new state in the same REPEATABLE READ snapshot as Phase 20 and emits steps in this
order.

Submit stage (the submitter):

1. `create principal` for each declared principal that does not exist.
2. `create group` (with `schedule_weight`).
3. `create membership` for each declared member without a live membership.
4. `propose role` for each declared role without a live grant.
5. The Phase 20 registry steps (connectors, tools, contracts, agents, versions, allowlists). They follow
   identity because an agent's owner may be a principal or group created above.
6. `create policy` when the declared content differs from the active version's content (compared as
   canonical JSON, JCS).
7. `create budget` for each declared account that does not exist, parents first.
8. `set budget` (a limit decrease) where `hard_limit` is below the current one, children first. The limit
   change trigger applies a decrease with one admin.
9. `propose budget` (a limit increase) where `hard_limit` is above the current one.
10. `set soft_limit` where the declared soft limit differs.
11. `create price` where the declared rates differ (numerically) from the price in effect, or none exists.
    It takes effect at apply time.

Approve stage (the approver):

1. `activate role` for each role proposed in submit.
2. `activate policy`.
3. `activate budget` (apply the increase), parents first, so each child's escrow comes from a parent that
   is already raised.
4. The Phase 20 activations and transitions.
5. With prune: `revoke role` for each live grant of a managed principal that is not declared, and
   `revoke membership` for each live membership of a managed group that is not declared.

New step op: `set` (next to `create`, `propose`, `activate`, `transition`, `revoke`, `import`).

Findings:

- `pending` (blocking): a live but unapproved grant for a declared or pruned role, or an open `PROPOSED`
  limit change on a declared account.
- `admin_floor` (blocking): after the plan's revokes, fewer than two approved, unrevoked `admin` grants
  would remain on enabled human principals. Only checked when the plan revokes an admin grant, so a tenant
  bootstrapped with one admin can still plan.
- `unsupported` (blocking): a change to an immutable field, a declared principal that is disabled, or a
  `schedule_weight` change.
- `unmanaged` (blocking): a declared object that exists but is not managed or imported (Phase 20 rule).
- `orphan` (reported): a managed principal, group, budget, price or policy no longer declared. Nothing is
  disabled, deleted or cleared.
- `unmanaged_reference` (reported): `members`, `owner`, `parent` or `agent` naming an existing object this
  bundle does not manage.

Refs: every object a step reads or moves is added to the change set's refs, as in Phase 20, including the
policy pointer and the budget accounts whose limits change.

## 6. Data model (migration 00021)

No new tables.

- `change_set_steps.op` accepts `set`. The step and ref kind CHECKs accept `principal`, `group`,
  `membership`, `grant`, `policy`, `budget`, `price`; steps also accept `soft_limit` (a soft limit is a
  separate write from a limit change, on the address `budget.<name>`). `bundle_resources.kind` accepts `principal`,
  `group`, `membership`, `grant`, `policy`, `budget`, `price`.
- `bundle_resources_one_policy`: a unique index on `(tenant_id)` where `kind = 'policy'`.
- `eacp.change_set_ref_row` (CREATE OR REPLACE) learns the new kinds:
  - `principal`: id, name, kind, subject, disabled, and its live grants (role, approved or not).
  - `group`: id, name, schedule weight, and its live memberships.
  - `membership`, `grant`: the row's identity and live state.
  - `policy`: the tenant pointer's current version id and number.
  - `budget`: id, name, unit, parent, agent, `hard_limit`, soft limit, and any open proposal id. It
    **excludes** `allocated`, `reserved` and `committed`, which move with every action and would make every
    plan stale.
  - `price`: the id of the price in effect for that provider and model.
- The step guard accepts the new op and kind pairs. It is otherwise unchanged: object ids written once, by
  the stage's transaction, in order.
- **Admin floor in PostgreSQL:** the stage's deferred commit trigger (`change_set_steps` completeness check)
  also refuses a stage that revoked an `admin` grant if fewer than two approved, unrevoked `admin` grants on
  enabled human principals remain at commit (SQLSTATE `23514`, mapped to `step_failed`). A concurrent API
  revoke that commits first is therefore caught too.
- Down restores the 00020 definitions of every replaced function and CHECK.

The RLS catalog is unchanged except for the replaced functions (none are SECURITY DEFINER).

## 7. Shared transactions (Go)

- `registry.Tx`: `CreatePrincipal`, `ProposeRole`, `ApproveRole`, `RevokeRole`, `CreateGroup` (with
  weight), `AddMember`, `RemoveMember`.
- `governance.Tx`: `CreatePolicy`, `ActivatePolicy`.
- `budget.Tx`: `CreateAccount`, `ChangeLimit`, `DecideLimitChange`.
- `finops.Tx`: `AddPrice` (always `effective_from = now()` when called from a bundle), `SetSoftLimit`.

The existing services (`registry.Service`, `governance.Store`, `budget.Service`, `finops.Service`) call
these, so their suites prove the refactor preserves behaviour.

## 8. Drift

Same statuses as Phase 20 (`in_sync`, `modified`, `missing`, `unmanaged_reference`). `modified` covers a
different set of approved roles, different live members, an active policy whose content differs, a
different `hard_limit` or soft limit, or a different price in effect.

## 9. API and CLI

No new routes: `/v1/change-sets` and `/v1/bundles/{name}/drift` carry the larger document. eacpctl inlines
`policy.file`, refusing a path outside the bundle directory or a file over 1 MiB.

## 10. Invariants (tested first)

1. A bundle can do only what the same admin could do through the API; each step passes its own trigger.
2. A role grant, a policy activation and a limit increase each need a second person (grant: ≠ proposer
   and ≠ grantee; policy: ≠ author; increase: ≠ proposer).
3. No change set leaves fewer than two admins when it revoked one, even under a concurrent API revoke.
4. A plan is stale if any principal, group, grant, membership, policy pointer, budget limit or price it
   depends on moved; action traffic alone never makes it stale.
5. Nothing is disabled, deleted or backdated; prune only revokes grants and removes memberships.
6. Change sets stay tenant-isolated (invariant 8) and every move stays journaled (invariant 17).

## 11. Testing

- Raw SQL as `eacp_app` (`internal/bundle/schema_test.go`): the new kinds and `set`, the one-policy index,
  the admin floor at commit (including a concurrent revoke), and `change_set_ref_row` ignoring budget
  counters.
- Planner unit tests per domain: order, immutable fields, pending, admin floor, orphans, prune, JCS policy
  compare, numeric price compare, escrow order.
- Service tests against PostgreSQL: a two-person grant; the grantee cannot approve their own grant; policy
  activation by a non-author; a budget created and raised in one change set; a decrease at submit; a
  soft limit; a forward-only price; staleness when a grant is approved concurrently; drift for each kind.
- The domain suites (`registry`, `governance`, `budget`, `finops`) stay green after the `Tx` refactor.
- API: a mixed bundle end to end; eacpctl: policy file inlining and path refusal.
- The Slice C demo's Governance-as-Code step gains a principal, a grant and a budget.
- `docs/INVARIANTS.md`: the new tests join invariants 8 and 17.

## 12. Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| A pending grant or limit change from the API | Blocks the plan; the bundle never approves someone else's proposal. |
| A declared principal who is disabled | Blocks (`unsupported`); a bundle never re-enables anyone. |
| A tenant with one admin revoking none | Planned normally; the floor applies only to plans that revoke `admin`. |
| Soft limit omitted | Left alone; clearing stays an API action. |
| Price history | Only additions effective at apply time; the bundle never removes or backdates a price. |
