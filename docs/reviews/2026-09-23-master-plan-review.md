# EACP Master Plan — Critical Review

- **Reviewed document:** `docs/MASTER_PLAN.md` (originally `EACP_Master Development Plan.md`) (revision 2026-09-23)
- **Review date:** 2026-09-23
- **Reviewers:** Claude Code (primary), Codex (cross-review; see §5)
- **Status:** Applied. The owner accepted the recommendations on 2026-09-23. They are incorporated in `docs/MASTER_PLAN.md` **Revision 2** and in ADR-001, ADR-002, ADR-004 and ADR-005.
- **Note:** §N references in this review point to **Revision 1** section content. Phase sections §75–§97 were renumbered in Rev 2.

Section references like **§15** point to the numbered sections of the master plan.

---

## 1. Summary

The plan's direction is sound. It splits into control, governance and execution planes, treats `UNKNOWN_OUTCOME` as a first-class state, refuses to claim fake exactly-once, uses lease + fencing and the transactional outbox, and keeps an extension-first stance toward AGT. Those are the right instincts.

The review found three kinds of problems that should be fixed **before** the Phase 0 ADRs are written:

1. **The upstream assumptions about Microsoft AGT/ACS are partly wrong.** ACS has no Go binding, the AGT Go SDK has no approval chains, and ACS is stateless, so EACP has to own approval state. That changes ADR-002, ADR-005 and Phase 3.
2. **One correctness hole undermines the flagship guarantee.** Fencing tokens protect *database commits*, not *external side effects*. As written, a stale worker can still create a second Purchase Order.
3. **Scope and sequencing.** The MVP (§110) is close to the whole system, and the portfolio demo (§111) needs features scheduled after the MVP.

The findings are ranked **Critical → Low**. §6 lists the decisions to make before Phase 0 starts.

---

## 2. Verified upstream facts (as of 2026-09-23)

§107 says "invent API without verifying upstream" is forbidden, so these were checked against primary sources:

| Fact | Consequence for EACP | Source |
|---|---|---|
| AGT is MIT-licensed, **Public Preview v4.1.0**, and may still break before GA. Packages: `agent-governance-toolkit-core`, `-runtime`, `-sre`, `-cli`. | Pin the version and write conformance tests for the adapter (§5 is already cautious here, which is good). | [AGT repo][agt-repo] |
| AGT has 5 SDKs. **Python is the full stack. Go (`agent-governance-golang`) covers core only**: policy, identity, trust, audit. | The Go control plane cannot use AGT runtime or SRE features natively. | [AGT repo][agt-repo] |
| **The Go SDK has no `require_approval` approval-chain execution layer and no ADR-0030 fail-closed action binding.** Issue #3083 is open. | The §14 "Action-Bound Approval" can't be taken from AGT in Go. | [Issue #3083][agt-3083] |
| **ACS is an in-process library.** It has a Rust core with C-ABI, PyO3, napi and P/Invoke bindings, and SDKs for Python, Node, .NET and Rust. **There's no Go ACS SDK.** | `MicrosoftAGTProvider` (§77) needs cgo or a sidecar. | [ACS docs][acs] |
| **ACS is stateless:** "the host supplies the complete snapshot for every call". | Approval requests, votes and one-time consumption **must be stored by EACP**. | [ACS docs][acs] |
| ACS verdicts are `allow / warn / deny / escalate / transform`. Its lifecycle hooks include `pre_tool_call` and `post_tool_call`. | `GovernanceDecision` (§12) has to model `warn`, `escalate` and `transform`, not just allow/deny. | [ACS docs][acs] |
| AGT action binding uses **JCS (RFC 8785) + SHA-256**, with two identities: `input_identity` and `enforced_identity` (post-transform). **Approval binds to `enforced_identity`.** | §14 has to digest the *enforced* action. | [AGT search summary: ADR-0030][agt-spec] |
| "Revalidation" is not an ACS primitive. | §12's `Revalidate()` has to be defined by EACP. | [ACS docs][acs] |

---

## 3. Findings

Format for each finding: **Sections**, **Problem**, **Failure scenario**, **Recommendation**, **Proposed plan/ADR change**.

### Critical

#### C1. There's no native way to call AGT/ACS from Go
- **Sections:** §4, §5, §12, §77, §110
- **Problem:** The plan assumes a Go `MicrosoftAGTProvider` as the primary integration. ACS has no Go binding, and the Go AGT SDK is missing approval chains.
- **Failure scenario:** Phase 3 starts and finds there's no Go API to call. The team then either hand-writes cgo against a preview C-ABI that may break at any release, or quietly reimplements AGT semantics, which violates §4/§5.
- **Recommendation:** Run AGT as a **sidecar PDP**: a thin Python (full stack) or Rust service exposing `Evaluate` over HTTP/gRPC, loopback only, with mTLS. The Go side talks to it through the generic "Custom HTTP PDP" provider slot that §12 already lists. Ship a `local` deterministic provider first, so Phases 4–10 aren't blocked on AGT.
- **Proposed change:** ADR-002 should pick one of (a) sidecar PDP, (b) cgo against the ACS C-ABI, or (c) Go SDK core plus EACP-owned approval. It should also document the latency budget and fail-closed behaviour when the sidecar is unreachable (→ `deny`).

#### C2. Approval ownership contradicts itself
- **Sections:** §13 ("do not duplicate governance engine state"), §14, §15 ("consume one-time approval" inside the Postgres transaction), §109 ("PostgreSQL ApprovalStore")
- **Problem:** ACS is stateless and the Go SDK has no approval store, so no AGT-side approval state exists to "not duplicate". Separately, §15 needs approval consumption to happen **inside the same Postgres transaction** as idempotency, budget and the outbox. That's only possible if the approval lives in EACP's database.
- **Failure scenario:** If approvals lived in an external AGT store, "consume approval" and "create action" become a distributed transaction. A crash between them either burns the approval without an action or creates an action with the approval still reusable, which breaks invariant 2 (§103).
- **Recommendation:** State it explicitly: **AGT decides; EACP stores and consumes.** EACP owns `approval_requests`, `approval_votes` and `approval_grants`, with a `consumed_at` and `consumed_by_action_id` unique constraint, and does the consumption with `UPDATE … WHERE consumed_at IS NULL` inside the §15 transaction.
- **Proposed change:** Rewrite §13 as "reference governance *decisions*; own approval *state*". This becomes the core of ADR-005.

#### C3. The action digest must bind to the post-transform action
- **Sections:** §14, §33
- **Problem:** §14 digests the request as submitted. ACS can return `transform`, for example to redact a field or cap an amount. AGT binds approval to `enforced_identity`, the post-transform digest.
- **Failure scenario:** The agent requests a PO for 24M THB. Policy transforms it to cap at 2.4M and escalates. The approver sees and approves 2.4M, but the approval is keyed to the 24M digest. Depending on which payload the worker executes, either the approval never matches, or the approver consented to something other than what runs.
- **Recommendation:** Store `input_digest` and `enforced_digest`. Approval, idempotency conflict checks and execution all use `enforced_digest`, and the worker executes **only** the enforced payload that was persisted in the atomic boundary.
- **Proposed change:** Update §14 and ADR-005. Add a security test called "transform-then-approve binds to enforced payload".

#### C4. Fencing doesn't stop a stale worker from repeating an external side effect
- **Sections:** §22, §23, §103 (invariants 1 and 4), §19
- **Problem:** The fencing token in §23 makes a stale worker's **DB commit** fail. It doesn't stop the stale worker's **external call**. The plan's invariants read as if the double effect is prevented, but it isn't.
- **Failure scenario:** Worker A (gen 10) sends `create_po` to SAP, then hits a GC pause or network partition. The lease expires. Worker B (gen 11) reclaims the action, sees state `EXECUTING`, and dispatches `create_po` again. **Two POs.** A's later commit is correctly rejected, but by then the damage is done.
- **Recommendation:**
  1. Write a durable `DISPATCH_INTENT` journal row, fenced by generation, in its own commit **before** any external call.
  2. **Reclaim rule:** if the journal shows dispatch intent for this action and the connector isn't `native`-idempotent, the new lease holder moves the action to `UNKNOWN_OUTCOME → RECONCILING`. It **never** re-executes.
  3. Always pass a stable, action-scoped idempotency key to the connector (derived from `action_id`, not the attempt). Pass the fencing generation too where the target supports conditional writes.
  4. Worker crash with no response counts as `UNKNOWN_OUTCOME`, not just timeouts (see H2).
- **Proposed change:** Add to §103: "*A reclaimed action whose dispatch intent is recorded is reconciled, never blindly re-dispatched.*" This becomes the core test for ADR-007 and ADR-009, and belongs in the §111 demo ("kill worker mid-dispatch").

#### C5. Nothing enforces "access through the Control Plane" until Phase 22
- **Sections:** §1 (core principle), §67, §70, §96
- **Problem:** The whole value proposition rests on agents *having* to go through EACP to reach enterprise resources. The only mechanism that makes that true, workers holding credentials and agents never having them, is scheduled for Phase 22.
- **Failure scenario:** A team gives its agent the SAP service account directly. Registry, governance, budget, kill switch and audit all see nothing. The "blast radius" answer is silently wrong.
- **Recommendation:** Promote a minimal version into the MVP. Connector credentials live **only** in worker config or a secret store, agents authenticate to EACP with their own identity, and the docs state the deployment assumption plainly (network egress policy, credential custody). JIT credentials stay in Phase 22.
- **Proposed change:** Add a new ADR called "Enforcement Point & Credential Custody", and add a line to §110.

### High

#### H1. Two sources of truth for dispatch (Postgres vs NATS)
- **Sections:** §24, §59, §60, §79, §62
- **Problem:** Actions and leases are authoritative in Postgres, but §60 also uses NATS JetStream for "action dispatch". If workers pull work from NATS, delivery order comes from JetStream, and the fair scheduler in §24 is bypassed. There are also two queues to keep consistent.
- **Failure scenario:** Team A floods the NATS subject with 10,000 messages. Workers consume in arrival order and Team B starves, even though the scheduler "exists".
- **Recommendation:** For the MVP, **Postgres is the queue.** The scheduler selects with `FOR UPDATE SKIP LOCKED` and weighted DRR, or River, which §73 already lists for study. NATS carries only non-authoritative signals: "work available" wake-ups, kill fan-out, and events for dashboards. Workers always claim through Postgres.
- **Proposed change:** Record this in ADR-014 and reword the "action dispatch" bullet in §60.

#### H2. Gaps in the action state machine
- **Sections:** §15, §18, §58
- **Problems:**
  - There's no `PENDING_APPROVAL` state for async `escalate` verdicts, which can take hours.
  - §15 creates the action *after* governance allows it, but §18 starts at `CREATED → AUTHORIZED`. Decide whether denied or pending requests are persisted as actions. They probably should be, for audit.
  - The `ADMITTED → QUEUED → LEASED` ordering relative to the scheduler is unclear.
  - `PREPARING → EXECUTING`: "executing" should mean "dispatch intent recorded" (see C4).
  - A crash in `EXECUTING` has no defined path. It should be lease expiry → `UNKNOWN_OUTCOME`.
  - `Action` vs `ActionAttempt` (§58) and how attempts relate to lease generation isn't defined.
- **Recommendation:** Write ADR-004 as a full transition table: from, to, trigger, guard, who may perform it, audit event. Test it property-based, e.g. no path from `DENIED` to `EXECUTING`, and every terminal state is reachable.

#### H3. There's no synchronous result path for agents
- **Sections:** §17, §64, §65
- **Problem:** Most agent frameworks call a tool and wait for the result, but EACP is fully asynchronous. There's no API contract for getting results back, and no latency target for the "happy path".
- **Failure scenario:** A LangGraph agent's tool call returns `202 Accepted` and the framework can't continue, so teams bypass EACP (see C5).
- **Recommendation:** `POST /v1/actions` with `wait=<duration>` (bounded long-poll) returns the result if it finishes inside the window, or `202` plus `action_id`. Add SSE or a webhook for long-running actions. Set a target control-plane overhead SLO (§105 already separates overhead from external latency, so give it a number).

#### H4. It's unclear whether EACP sits in the LLM call path
- **Sections:** §45–§48, §2, §6
- **Problem:** FinOps needs LLM token and cost data, but the plan never says whether EACP proxies LLM calls (an AI gateway) or only governs tool actions. That's the biggest undecided product boundary.
- **Recommendation:** Decide in ADR-001. A reasonable MVP: **tool and actions only.** LLM cost is *reported* by agents or ingested from provider billing and OTel GenAI spans. An LLM gateway is a later, optional module.

#### H5. Execution semantics come from untrusted metadata
- **Sections:** §29, §31, §32, §67
- **Problem:** §67 says tool and MCP metadata are untrusted, yet retry safety (§29) depends on `side_effect` and `idempotency.mode`, which look like they're self-declared by the connector.
- **Failure scenario:** A malicious or buggy MCP server declares `create_po` as `READ_ONLY`, so EACP retries it freely and creates duplicate POs.
- **Recommendation:** The side-effect class and idempotency mode are **operator-declared** in the Tool Registry. They're bound to the tool fingerprint (§33), and any fingerprint change invalidates them. Server self-descriptions are advisory only. When unknown, default to the most restrictive class (`IRREVERSIBLE` + non-idempotent → at-most-once).

#### H6. Hierarchical budget reservation causes hot-row contention and reservation leaks
- **Sections:** §15, §46, §47, §85
- **Problem:** Reserving across Org → BU → Dept → Team → Agent inside **every** atomic boundary means every action locks the same Org row. Reservations held by crashed flows are never released.
- **Recommendation:** Use a fixed lock order (root → leaf) to avoid deadlocks. Use escrow or sub-allocation, where parents pre-allocate quota to children so the hot path locks only leaf rows. Give reservations `expires_at` and add a sweeper. Only *hard* budgets are in the atomic boundary; soft budgets are async. The 100-way concurrency test in §85 should also measure p99 under contention.

#### H7. The kill switch has a TOCTOU gap
- **Sections:** §39, §40
- **Problem:** "Check before execute" followed by execute is a race: a kill can land in between. NATS kill events can also be lost or delayed.
- **Recommendation:** Keep a monotonically increasing `kill_epoch` per scope in Postgres, and check it **in the same transaction** as the transition to dispatch intent. Workers poll the epoch on a short interval as a backstop to the NATS push. The plan's "cancel != rollback" point stands, so a kill during dispatch goes to reconciliation.

#### H8. Idempotency semantics aren't specified
- **Sections:** §15, §63, §69
- **Recommendation:** Scope keys to `(tenant_id, agent_id, idempotency_key)`. The same key with the same `enforced_digest` returns the existing action. The same key with a **different** digest returns `409 Conflict`. Set a retention TTL longer than the maximum retry window. Keep the key separate from the connector-facing idempotency key in C4. Do all of this in ADR-008.

### Medium

#### M1. The MVP is nearly the whole system, and the demo needs features outside it
- **Sections:** §110, §111, §75–§97
- **Problem:** §110 has about 18 capabilities. The §111 portfolio demo needs MCP drift and blast radius, which come from Phases 14–15, after most of the MVP.
- **Recommendation:** Resequence into thin vertical slices that each end in a demo:
  1. **Slice A, "Correct execution":** minimal registry, Action API, `local` governance provider, atomic boundary, outbox, lease/fencing worker, Fake ERP, `UNKNOWN_OUTCOME`, reconciliation. *Demo: kill a worker mid-dispatch, then reconcile.*
  2. **Slice B, "Governed execution":** the AGT sidecar provider, approval store, enforced-digest binding, and a hard budget.
  3. **Slice C, "Fleet safety":** tool fingerprints and drift, a dependency graph (recursive CTE), blast radius, and the kill switch.
  4. Then fair scheduler benchmarks, backpressure, circuit breakers, FinOps, release and SOC.

#### M2. The Temporal/River overlap isn't justified
- **Sections:** §73, §114
- **Problem:** Durable queue + lease + retry + journal is Temporal's core, and §114 says not to compete with Temporal. The plan never records *why* it builds its own.
- **Recommendation:** Add an ADR called "Build vs. adopt execution engine". Learning and portfolio value, domain-specific semantics (approval consumption and budget in the same transaction, UNKNOWN_OUTCOME as a first-class state) and avoiding a heavy dependency are all legitimate reasons, but they should be written down. Also consider River for the job-queue layer.

#### M3. The tenant isolation mechanism isn't specified
- **Sections:** §69
- **Recommendation:** Use Postgres Row-Level Security with `SET LOCAL app.tenant_id` per transaction, plus tenant-scoped unique keys for idempotency, budgets and kill scopes. §69's tests then run against RLS, not just application checks.

#### M4. Where circuit breaker state lives isn't decided
- **Sections:** §28, §27
- **Recommendation:** Pick per-worker breakers (simple, converge slowly) or shared state (Postgres or NATS KV). Given that §61 says Redis is not a source of truth, start per-worker and add a shared "connector disabled" flag driven by the kill switch or an operator.

#### M5. Replay and shadow assume determinism that LLM agents don't have
- **Sections:** §51, §52, §93
- **Recommendation:** Replay needs recorded tool responses (a record/replay connector) and accepts non-deterministic LLM output, so compare distributions, not exact results. Shadow needs connectors that are *structurally* unable to write, not just flagged. Mark §51–§52 research-grade and outside MVP.

### Low

- **L1.** "**AEF**" appears in §43, §108 and §115 but is never defined. Use "Execution Fabric" consistently.
- **L2.** The file has Markdown export artefacts (`\#`, `\_`, `&#x20;`, `\&`). Normalise it when it moves to `docs/MASTER_PLAN.md` (§71).
- **L3.** Metrics in §44 labelled by `agent_id` will be high-cardinality at 1k–10k agents (§104). Keep per-agent data in traces and logs, and keep metric labels bounded (tenant, connector, state).
- **L4.** OTel context has to propagate through the outbox row and NATS headers (W3C `traceparent`), or the §41 end-to-end trace breaks at the queue.
- **L5.** Outcome attestation (§43) should be hash-chained or signed, to address "audit manipulation" (§68). Reuse AGT audit primitives where possible.
- **L6.** §23's SQL example should also check `state = 'EXECUTING'` and `worker_id`, not only `lease_generation`, so that state regressions are fenced too.

---

## 4. What the plan gets right (keep these)

- `UNKNOWN_OUTCOME` as a first-class state, and "timeout ≠ failure" (§19).
- "No fake exactly-once" terminology (§21) and "never fake benchmarks" (§105).
- Transactional outbox plus inbox dedup, assuming duplicate messages (§60, §62, §63).
- Retry decisions driven by side-effect class (§29), once H5 is fixed.
- Recursive CTE before a graph database (§37). Don't split into microservices early (§72).
- The critical invariants list (§103). It's excellent. Extend it with C4 and C3.
- The §106/§107 development rules, especially "do not implement the next phase automatically".

---

## 5. Codex cross-review

**How it ran:** Codex CLI 0.153.4, `codex exec -s read-only -m gpt-5.5`, with the master plan and this draft inlined in the prompt. The configured default model `gpt-6-sol` was rejected by the API for ChatGPT-account auth, and the read-only sandbox couldn't read files on Windows, so the content was inlined. What follows is Codex's output, condensed, with an adjudication for each point.

### 5.1 New findings from Codex

| ID | Sev. | Finding (Codex) | Sections | Adjudication |
|---|---|---|---|---|
| X1 | Critical | **The governance PDP's failure domain is underspecified.** A sidecar could return a partial, stale, replayed or wrong-bundle `allow`, and EACP would keep no durable proof of which policy evaluated the action. Codex recommends persisting `policy_bundle_id`, `policy_version`, PDP identity, decision timestamp, a nonce and hash-linked decision evidence inside the atomic boundary, and failing closed when the evidence is missing. | §12, §15, §77, §103 | **Accept.** This complements C1 and C2 and becomes part of ADR-002/005. Invariant 10 (§103) can't hold without it. |
| X2 | High | **Policy changes can leave pending approvals stale.** Approval is requested under v1, v2 would deny the action, and the old grant gets consumed hours later. Codex recommends binding grants to `policy_version` + `enforced_digest`, with revalidate-or-deny as the default for high-risk actions. | §13, §14, §18 | **Accept.** It gives concrete meaning to §12's undefined `Revalidate()`: re-evaluate at consumption, and deny when the policy version has changed and the verdict differs. |
| X3 | High | **Approver authority and separation of duties aren't modelled.** The agent's owner could approve its own action, or an approver from another tenant or BU could. Codex recommends making approver eligibility a policy (tenant, role, threshold, conflict of interest, quorum, expiry, delegation) and storing the authorization basis with each vote. | §14, §69 | **Accept.** Because C2 moves approval state into EACP, EACP must also enforce eligibility. |
| X4 | High | **Reconciliation can itself cause a duplicate.** Delayed indexing in SAP produces a false "not found", the action is retried, and a duplicate PO is created. Codex recommends connector-specific, conservative reconciliation that needs stable external correlation IDs; if the evidence is insufficient, go to `NEEDS_HUMAN_RESOLUTION`. | §19, §20, §22 | **Accept. This is an important catch.** `CONFIRMED_NOT_EXECUTED` in §20 needs a *proof standard* per connector, not just "not found". Add it to ADR-010 and §103. |
| X5 | Medium | **Blast-radius data may be stale exactly when it's needed.** Codex recommends freshness, source and confidence on graph edges, and treating unknown or stale edges as a *wider* blast radius. | §36, §37 | **Accept.** It belongs in ADR-015. |
| X6 | Medium | **Data residency, redaction and retention are schema-level decisions, not later polish.** Traces and audit logs carry prompts, arguments and PII. | §41, §43, §69, §113 | **Accept**, with one scoping note: decide redaction and tenant partitioning in Phase 0. Residency and legal hold can stay in §113. |
| X7 | Medium | **Operator and admin actions need governance too.** Disabling a kill switch, editing a budget or changing tool metadata should require dual control and immutable audit, with a break-glass path. | §39, §40, §57, §68 | **Accept.** Model privileged control-plane mutations as governed actions. |

### 5.2 Codex's disagreements with this draft

| Draft ID | Codex position | Adjudication |
|---|---|---|
| C1 | Direction is right, but "PDP unreachable → deny" is too blunt. Reconciliation, status reads and kill-switch propagation must not be blocked when the PDP is down. | **Accept.** ADR-002 defines failure behaviour per decision type: new side-effecting actions fail closed, while safety and recovery paths (kill, reconcile reads, status) don't depend on the PDP. |
| C4 | Correct, but it should also require per-connector execution contracts (native idempotency, correlation lookup, reconciliation guarantees, retry permission, human-resolution behaviour) as part of connector certification. | **Accept.** Merge into ADR-013 (Connector Contract) and §34 certification. |
| H1 | Overstated. NATS dispatch is fine if messages are only claim hints carrying `action_id`. The real rule is that workers never execute from NATS alone, and every execution is claimed and fenced through Postgres. | **Partly accept.** The invariant is exactly as Codex states. Fairness is still the reason the scheduler must choose *which* action is claimed from Postgres: a NATS hint can wake a worker, but the claim query decides the order. Reworded as: *"Postgres is the sole execution authority; NATS carries hints/signals only."* |
| H2 | Understated. The state machine is the correctness boundary for approvals, budgets, kill, retries and audit, so ADR-004 should be a hard gate for Phase 0. | **Accept.** H2 is raised to Critical-gate. |
| H3 + H4 | Should be merged into one decision: which call paths must pass through EACP on day one. | **Accept.** They're merged into decision #1 below, together with C5. |
| M3 | Tenant isolation should be High, not Medium. Cross-tenant leakage blocks launch. | **Accept.** M3 is raised to High. |

### 5.3 Codex's top decisions

1. Product boundary and enforcement point (tool gateway vs. LLM gateway; what forces agents through EACP).
2. Authority model (policy decisions, approval state, approver eligibility, tenant boundary, policy-version binding).
3. Execution correctness contract (authorization → dispatch → lease loss → crash → unknown outcome → reconciliation → human resolution).
4. Queue and scheduler authority (Postgres as sole execution authority).
5. MVP built around one defensible guarantee: governed, fenced, auditable execution of one side-effecting connector, including kill-mid-dispatch and reconciliation.

These line up closely with this review's §6, which has been updated to include X1–X4 and the severity changes.

---

## 6. Recommended decisions before Phase 0

Severity changes after the cross-review: **H2 → Critical (Phase 0 gate)** and **M3 → High**. H1 is reworded (see §5.2).

| # | Decision | ADR | Driven by |
|---|---|---|---|
| 1 | Product boundary and enforcement point: which call paths must go through EACP on day one (tool gateway vs. LLM gateway), the sync result API, and credential custody | ADR-001 + **new ADR-019** | C5, H3, H4 |
| 2 | AGT integration mode (sidecar PDP recommended, `local` provider first) and per-decision-type PDP failure behaviour | ADR-002 | C1, X1 |
| 3 | Authority model: EACP owns approval state; grants bind to `enforced_digest` + `policy_version`; approver eligibility and separation of duties; persisted decision evidence | ADR-005 (+ §13, §14 edits) | C2, C3, X1, X2, X3 |
| 4 | **Gate:** full action state transition table, including `PENDING_APPROVAL` and `NEEDS_HUMAN_RESOLUTION` | ADR-004 | H2 |
| 5 | Execution correctness contract: dispatch-intent journal, reclaim-to-reconcile, per-connector reconciliation proof standard | ADR-007 / 009 / 010 / 013 | C4, X4 |
| 6 | Postgres is the sole execution authority; NATS carries hints and signals only | ADR-014 | H1 |
| 7 | Tenant isolation (RLS) and data redaction/partitioning in the schema | new ADR (or THREAT_MODEL) | M3, X6 |
| 8 | **New:** build vs. adopt execution engine (Temporal/River) | ADR-020 | M2 |
| 9 | Resequence the MVP into slices A/B/C, with slice A as the "one defensible guarantee" | Master plan §110 | M1, Codex #5 |

---

## Sources

- [microsoft/agent-governance-toolkit (GitHub)][agt-repo]
- [Agent Control Specification docs][acs]
- [Issue #3083 — language parity for require_approval approval chains][agt-3083]
- [AGT policy-engine SPECIFICATION.md][agt-spec]
- [AGT Discussion #276 — policy enforcement vs decision evidence][agt-276]

[agt-repo]: https://github.com/microsoft/agent-governance-toolkit
[acs]: https://microsoft.github.io/agent-governance-toolkit/packages/agent-control-specification/
[agt-3083]: https://github.com/microsoft/agent-governance-toolkit/issues/3083
[agt-spec]: https://github.com/microsoft/agent-governance-toolkit/blob/main/policy-engine/spec/SPECIFICATION.md
[agt-276]: https://github.com/microsoft/agent-governance-toolkit/discussions/276
