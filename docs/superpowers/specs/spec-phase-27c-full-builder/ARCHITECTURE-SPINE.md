---
title: Phase 27c architecture spine
status: accepted
date: 2026-10-01
updated: 2026-10-01
---

# Phase 27c architecture spine

## Paradigm

Database-enforced governance with an API-only leased interpreter. The immutable definition describes a bounded forward graph; PostgreSQL owns the execution cursor and admits all external work. The browser and runtime cannot grant a capability or decide containment.

## Inherited invariants

- ADR-001: agents hold no enterprise credential.
- ADR-003/033: registry rules, derived capabilities and distinct-person approvals live in PostgreSQL.
- ADR-005: no PDP call inside a database transaction.
- ADR-012/031: database reservations and settlement own hard LLM budgets; uncertain usage consumes its full reservation.
- ADR-016 Rev 1.1: authenticated run binding and permanent containment after a run fails `killed`.
- ADR-028: memory-only page credentials, safe DOM, literal routes, confirmed writes and English/Thai catalogue.
- ADR-029: replicas coordinate by leases, row locks and compare-and-set, never a message or elected leader.

## Decisions

### AD-1: Versioned definition and forward graph

**Binds:** PostgreSQL validation, runtime and builder.
**Prevents:** Different graph semantics or reinterpretation of saved v1 agents.
**Rule:** Preserve v1. V2 uses the graph, limits, branch comparison and dominance rules in `design.md` section 2. Reject unknown versions, keys and operators. Derive capabilities from every node, not only the selected path.

### AD-2: Server-owned progress

**Binds:** Studio functions, action insertion and runtime recovery.
**Prevents:** Jumping to an unchosen tool, premature success or lost progress after a crash.
**Rule:** V2 advances only its current node under the live runtime lease. An unfinished tool/LLM step cannot advance. Branch completion records its choice atomically with the cursor; success requires a terminal response and all visited external work to have succeeded.

### AD-3: One-use LLM admission

**Binds:** Runtime, gateway and LLM ledger.
**Prevents:** Unbound Studio calls and repeated billed calls after takeover or network failure.
**Rule:** An authenticated Studio key requires a current LLM step intent whose version, subject, model, provider and output cap match. Admission consumes it and links one ledger call in the same transaction. Re-fencing is allowed only before consumption. A consumed intent never yields permission to forward again.

### AD-4: Atomic typed result

**Binds:** Gateway settlement, Studio output storage and runtime.
**Prevents:** Advancing before a durable result exists or persisting provider envelopes.
**Rule:** Only the gateway stores a schema-valid typed result, atomically with successful settlement of its bound call. A stale, killed, terminal or mismatched run cannot acquire new output. No content enters audit, outbox, logs or operator APIs. Clear output at terminal state or deadline.

### AD-5: Preview cannot dispatch tools

**Binds:** Run creation, action insertion, gateway and builder tests.
**Prevents:** Draft or preview execution bypassing capability approval.
**Rule:** Draft preview is local and sample-only. Real LLM preview uses an approved owned immutable version, its approved derived key and sample tool values. A database-owned preview mode refuses all tool actions, even with a compromised runtime.

### AD-6: Credentials and budgets stay at existing boundaries

**Binds:** Runtime configuration and Studio admission.
**Prevents:** A second credential store, an unreserved Studio model call or author-selected spending from another account.
**Rule:** Runtime calls API/gateway with its derived agent key and holds no DB/provider/connector credential. V2 LLM runs require the agent's existing leaf budget in the model price's unit. Prices, budgets and approvals are provisioned through their existing authorized paths.

### AD-7: Schema validation cannot perform I/O

**Binds:** Definition validation and JSON output validation.
**Prevents:** SSRF and different runtime/gateway schema interpretations.
**Rule:** Use only the closed schema subset in `design.md`; no references, dynamic references or fetched schemas. Reuse the already pinned `github.com/google/jsonschema-go` without dependency changes.

### AD-8: Deployment preserves credential isolation

**Binds:** Compose, Helm and development demo resources.
**Prevents:** Runtime access to providers/DB or provider credentials in another pod.
**Rule:** Runtime reaches API and gateway only. Gateway reaches DB, PDP and explicitly configured provider peers only, plus necessary DNS. Optional gateway packaging is off by default, uses its own tokenless ServiceAccount and mounts a provider Secret by name. PostgreSQL/NATS remain outside the chart.

## Deferred

Inbound A2A, global platform authority, general DAG workflows, schedules, streaming Studio LLM calls, persisted free-position canvas geometry, SSO and network-resolved schemas remain outside this phase. Department usability feedback remains deferred until a real trial; it does not change the phase's correctness gates.

## Approval state

Accepted by the owner on 2026-10-01. The accepted ADRs are still the authority. No independent Codex review is dispatched, in accordance with the owner's standing rule; mechanical checks and source reconciliation do not substitute for such a review.
