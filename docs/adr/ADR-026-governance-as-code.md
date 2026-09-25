# ADR-026 — Governance-as-Code: Bundles, Plans, Change Sets and Drift

Status: Accepted (Rev 1.0) · Phase 20 · Date: 2026-09-25

## Context

Registry owners want the governed estate (connectors, tools, contracts, agents, versions, allowlists)
in files under review in Git, with a plan they can read before anything changes. Terraform (plan, apply,
state, import, drift) and Databricks Asset Bundles (a bundle directory, targets, variables, validate and
deploy) are the models. EACP must not gain a second authority: PostgreSQL triggers decide every registry
write (ADR-003 §8), and two-person rules stay two-person.

## Decision

1. **A bundle is desired state, resolved by the client.** `eacpctl bundle` reads `eacp.yml` and
   `resources/*.yml`, picks a target (API URL, tenant, variable values), resolves `${var.NAME}` and sends
   one JSON document. The server accepts JSON only, decodes it strictly (an unknown field is refused, so a
   secret cannot ride along) and refuses any unresolved `${`. A bundle never contains a secret value;
   connectors carry `secret_ref` only.
2. **PostgreSQL is the state.** `eacp.bundle_resources` maps a bundle's addresses (`connector.erp`,
   `tool.erp.create_po`, `agent.buyer`, `version.buyer`) to objects. An object belongs to at most one
   bundle. There is no state file; the open change set (one per bundle) is the lock.
3. **A plan is recorded, sealed and never writes the registry.** The API diffs the document against the
   registry in one REPEATABLE READ snapshot and records a change set: its canonical document, ordered
   steps (`create`, `propose`, `activate`, `transition`, `revoke`, `import`), each in stage `submit` or
   `approve`, and the refs it read. PostgreSQL computes `desired_digest` and `base_digest` (over the refs
   and the bundle's managed objects) and journals `change_set.planned`. A blocking finding records nothing.
4. **Apply is two stages, two people.** Submit locks the change set, checks `base_digest` (stale:
   `40001`), runs the submit steps as the submitter through `registry.Tx`, the same SQL as the API, then
   seals `sealed_digest` over the refs plus every object it produced. Approve needs a registry approver or
   admin who is not the submitter, checks `sealed_digest`, and runs the activations and transitions. Each
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
| Principals, roles, policies and budgets | Phase 21, on the same engine. |
