# Phase 29 architecture spine

## AD-01 — project the shared action engine

**Binds:** CAP-02, CAP-03. **Prevents:** a second queue, lease, state machine or authority. **Rule:** task ID equals action UUID; all writes go through the existing engine and PostgreSQL guards. A task projection never decides execution.

## AD-02 — reuse approved agent identity

**Binds:** CAP-01, CAP-02, CAP-03. **Prevents:** remote self-asserted tenant/subject authority and principal-key execution. **Rule:** reuse the existing agent wrapper; supplied tenant selectors cannot override the key. No new trust mechanism or credentials.

## AD-03 — bounded structured delegation

**Binds:** CAP-02. **Prevents:** speculative planning, ambiguous replay semantics and multi-turn state. **Rule:** one JSON data part, asynchronous delivery, fixed one-hour action lifetime and existing canonical core digest. Hash only the bounded message ID into the idempotency key; never hash payload into the key, which would create a new action on a changed replay.

## AD-04 — preserve uncertainty and private results

**Binds:** CAP-03, CAP-04. **Prevents:** reporting an ambiguous effect as completed/canceled, input disclosure and result access through task metadata. **Rule:** unknown/human-resolution action states project as working; cancellation succeeds only for actual `CANCELLED`; artifacts require `Engine.Result` as the authenticated agent. No history, content logs or raw error replies.

## Inherited invariants

ADR-001 credential boundary; ADR-003 registry and lifecycle; ADR-004 dispatch, unknown outcomes and operator resolution; ADR-005 no PDP call in a transaction; ADR-012 budgets; ADR-016 kill state; ADR-029 replica semantics; ADR-030 outbound single-attempt behavior; ADR-034 private retained output. Ingress does not alter any of them.

## Deployment and deferred scope

Opt-in public URL on the existing API listener. Compose and Helm can advertise their development API endpoint for live verification; network policies need no new destination or port. Default is disabled. Any broader identity federation, workflow skill, streaming or push support requires a later explicit phase/ADR.
