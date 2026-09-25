# ADR-026 — Governance-as-Code: Bundles, Plans, Change Sets and Drift

Status: Accepted (Rev 1.1) · Phases 20–21 · Date: 2026-09-25

## Context

Registry owners want the governed estate (connectors, tools, contracts, agents, versions, allowlists)
in files under review in Git, with a plan they can read before anything changes. Terraform (plan, apply,
state, import, drift) and Databricks Asset Bundles (a bundle directory, targets, variables, validate and
deploy) are the models. EACP must not gain a second authority: PostgreSQL triggers decide every registry
write (ADR-003 §8), and two-person rules stay two-person.

## Decision

1. **A bundle is desired state, resolved by the client.** `eacpctl bundle` reads `eacp.yml` and
   `resources/*.yml`, picks a target (API URL, tenant, variable values), resolves `${var.NAME}` and sends
   one JSON document. A target's API URL only asserts where the target lives: it must equal
   `EACP_API_URL`, so a bundle from a pull request never redirects the API key, and `approve`, `status`
   and `reject` never read a bundle. The server accepts JSON only, decodes it strictly (an unknown field is refused, so a
   secret cannot ride along) and refuses any unresolved `${`. A bundle never contains a secret value;
   connectors carry `secret_ref` only.
2. **PostgreSQL is the state.** `eacp.bundle_resources` maps a bundle's addresses (`connector.erp`,
   `tool.erp.create_po`, `agent.buyer`, `version.buyer`) to objects. An object belongs to at most one
   bundle. There is no state file; the open change set (one per bundle) is the lock.
3. **A plan is recorded, sealed and never writes the registry.** The API diffs the document against the
   registry in one REPEATABLE READ snapshot and records a change set: its canonical document, ordered
   steps (`create`, `propose`, `activate`, `transition`, `revoke`, `import`), each in stage `submit` or
   `approve`, and the refs it read. PostgreSQL computes `desired_digest` and `base_digest` (over the refs
   and the bundle's managed objects, each with its current fields and state) and journals `change_set.planned`. A blocking finding records nothing.
4. **Apply is two stages, two people.** Submit locks the change set, checks `base_digest` (stale:
   `40001`), runs the submit steps as the submitter through `registry.Tx`, the same SQL as the API, then
   seals `sealed_digest` over the refs plus every object it produced. Approve needs a registry approver or
   admin who is not the submitter, checks `sealed_digest`, and runs the activations and transitions. A
   transition moves a version only from the state it was planned from (`registry.Tx.TransitionVersionFrom`),
   so a move committed between the digest check and the step makes the change set stale. Each
   step still passes its own trigger (roles, ≠ creator, ≠ allowlist author, MCP definition pin, the
   one-`ACTIVE` release guard). A stage is one transaction: any failing step rolls it all back. A change
   set without approve steps applies at submit, as the same single-person writes do through the API.
5. **Nothing is deleted or replaced.** A change to an immutable field is `unsupported`. A managed object
   missing from the document is an `orphan`; with `prune` it becomes an existing lifecycle move only
   (retire a REGISTERED or SUSPENDED version, revoke a contract). An ACTIVE version is never retired by a
   bundle.
6. **Bundles do not bypass releases or containment.** Activating a version while another of the agent is
   ACTIVE is `requires_release` (ADR-018). A SUSPENDED or QUARANTINED version, or a quarantined MCP tool,
   is `contained`: the bundle never resumes or releases it (ADR-024, ADR-023). MCP tools are discovered,
   never declared; a bundle may pin the current definition in a contract.
7. **Drift observes.** `GET /v1/bundles/{name}/drift` compares the last applied document with the registry
   in a read-only snapshot and reports `in_sync`, `modified`, `missing` or `unmanaged_reference`. It never
   writes, blocks or remediates.

## Consequences

- Git review plus a recorded, digest-bound plan gives a reviewable path from a pull request to the
  registry, with every write still journaled by the registry and the change set journaled at each move.
- A plan made against one state cannot be applied to another: any change to a ref makes it stale.
- Upgrading a live agent stays a release; the bundle registers the new version and later adopts the
  promoted one by `code_ref`.
- The trust boundary is unchanged (ADR-003 §8): the guards stop application bugs, not a holder of a stolen
  `eacp_app` credential. A step's recorded object id is management metadata, never authority.

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Should a change set of single-person steps need approval? | No: it applies at submit, like the same API writes. A later ADR may tighten it. |
| Proposals of a rejected submission | They stay pending and inert; activating one still needs a second person. |
| Scope of the digest | The plan's refs and the bundle's managed objects, not the whole tenant. |
| Credentials in bundles | Excluded: they expire within 90 days and use bring-your-own-key hashes. |
| Principals, roles, policies and budgets | Phase 21, on the same engine (Revision 1.1). |
| A limit change or grant left open outside the bundle | It blocks the plan (`pending`); a person decides it through the API. A bundle never approves someone else's proposal. |
| Fewer than two admins | Never: the planner blocks it and `change_sets_commit` refuses the stage (HINT `admin_floor`). |

## Revision 1.1 (Phase 21): identity, policy, budgets and prices

A bundle may also declare `principals` (kind, subject, display name, roles), `groups` (display name,
schedule weight, members), the tenant `policy` (a local policy bundle), `budgets` (unit, parent, agent,
hard limit, soft limit) and `prices` (provider, model, unit and rates, keyed by a slug name because model
names contain dots). Their addresses are `principal.x`, `role.x.r`, `group.x`, `member.g.p`,
`policy.tenant`, `budget.x` and `price.x`; the address prefix is the ref kind the seal records.

- **Steps through the domain Tx types.** Every step writes through `registry.Tx`, `governance.Tx`,
  `budget.Tx` or `finops.Tx`, the same code the API services use, so the existing triggers decide it:
  every identity, policy, budget and price write needs an admin. A grant is proposed by the submitter and
  approved by the approver, who must be an admin other than the grantee. A policy version is created at
  submit and activated at approval. A limit decrease applies at submit and an increase is proposed at
  submit and applied at approval (parents first, so escrow holds). A soft limit is set at submit. A price
  is added, effective when the stage commits; it is never removed or backdated.
- **Order.** Submit: principals then their role proposals, groups then members, Phase 20 objects, the
  policy version, account creations (parents first), decreases (children first), increase proposals, soft
  limits, prices. Approval: role activations, Phase 20 activations, the policy activation, limit increases,
  Phase 20 prunes, then identity prunes.
- **Ownership.** A bundle manages the grants of the principals it manages and the memberships of the
  groups it manages. One bundle owns the tenant policy (`bundle_resources_one_policy`). A declared owner,
  member, parent account or budget agent may be created by the same change set (`Other`/`OtherID`).
- **Prune** revokes undeclared grants and removes undeclared memberships only; it never disables or
  enables a principal, clears a soft limit or removes a price.
- **Admin floor.** A plan that revokes an admin grant must leave at least two approved admins on enabled
  human principals; `change_sets_commit` checks the same at the stage's commit, so a concurrent API
  revoke cannot break it.
- **Digest.** Principal refs cover live grants, group refs live memberships, budget refs the limits, soft
  limit and open proposal, never the counters that move with every action. The policy ref of a tenant
  without a policy is the tenant's pointer, so any activation makes the plan stale. Amounts and policies
  are compared by value (numeric and canonical JSON), so reformatting plans nothing.
