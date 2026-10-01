# Enterprise Agent Control Plane

## Master Development Plan

- **Working name:** EACP
- **Full name:** Enterprise Agent Control Plane
- **Primary execution language:** Go
- **Governance foundation:** Microsoft Agent Governance Toolkit / Agent Control Specification (via sidecar PDP — see ADR-002)
- **Architecture style:** Control Plane + Governance Plane + Execution Plane
- **Target:** Enterprise AI Agent Infrastructure
- **Status:** Master Architecture Plan — **Revision 2** (post-review; ADR-001/002/004/005 are the normative detail)
- **Revision date:** 2026-09-23

---

# 0. Revision 2 — What Changed

Rev 2 adopts the findings of `docs/reviews/2026-09-23-master-plan-review.md` (a Claude + Codex cross-review).

Changed sections carry a **Rev 2** label. Section numbers are unchanged, so reviews and ADRs keep matching references.

Main decisions:

| # | Decision | ADR | Sections |
|---|---|---|---|
| 1 | EACP's first slice is an **Agent Tool / Action Control Plane**. It is not an LLM gateway yet, but its interfaces leave room for one | ADR-001 | §3.1, §45, §65 |
| 2 | **The enforcement point is in Slice A**: agents hold no credential for a privileged system, and side effects happen only through EACP or an execution proxy EACP controls | ADR-001 | §1, §3.2, §70 |
| 3 | AGT/ACS is connected through a **sidecar PDP** behind the `GovernanceProvider` interface; Go is the core | ADR-002 | §4, §5, §12, §83 |
| 4 | **AGT decides, EACP stores**: approval state lives in EACP's Postgres and is bound to `enforced_digest` + `policy_version` | ADR-005 | §13, §14, §15 |
| 5 | The full action state machine, with `PENDING_APPROVAL`, `NEEDS_HUMAN_RESOLUTION` and crash recovery for every state | ADR-004 | §18 |
| 6 | Fencing covers **execution semantics**, not only DB writes: dispatch intent, and reclaim → reconcile | ADR-004, ADR-005 | §19, §22, §23 |
| 7 | Operation identity and a reconciliation proof standard per connector. If all we get is "not found", a destructive action must not be retried automatically | ADR-004 | §20, §31 |
| 8 | Postgres is the only execution authority. NATS arrives in Slice B and carries hints/events only | ADR-014 | §60 |
| 9 | Tenant isolation uses Postgres RLS from the first schema | ADR-021 | §69 |
| 10 | The MVP is split into **Slice A / B / C**, and the first demo uses Slice A only | — | §73–§97, §110, §111 |

**Slice A goal statement:**

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

Where an assumption is still unresolved, choose the option that is **most conservative for correctness / safety** first.

"AEF" in the earlier documents means the **Execution Fabric** (the Execution Plane).

---

# 1. Vision

Enterprise Agent Control Plane is a central system for organisations that run many AI agents.

The goal is not to build a new agent framework.

It is to let a company:

```text
Discover
Govern
Authorize
Schedule
Execute
Observe
Audit
Control
Contain
Evaluate
Deploy
```

AI agents from many teams, many frameworks and many environments, through one central system.

The core idea:

> Teams may build agents however they want, but enterprise resources are accessed through the Control Plane.

Or:

> Build your agent with anything you like, but to use the organisation's resources it must go through the Control Plane.

**Rev 2 — this principle must actually be enforced, not just a convention:**

> Agents must hold no credential for calling a privileged external system directly.
> Every privileged side effect must execute through EACP or an execution proxy that EACP controls.

Details are in §3.2 and §70.

---

# 2. Problem

One organisation may have:

```text
Finance          120 Agents
Procurement       80 Agents
Engineering      450 Agents
Sales            170 Agents
HR                60 Agents
Security          90 Agents
Customer Service 300 Agents

Total          1,270 Agents
```

These agents may be built with:

```text
OpenAI Agents
Claude
LangGraph
Microsoft Agent Framework
AutoGen
CrewAI
Custom Python
Custom Go
Internal platforms
```

And connect to:

```text
SAP
Salesforce
Databases
GitHub
Email
Slack / Teams
Cloud infrastructure
MCP Servers
A2A Agents
LLM Providers
Internal APIs
```

If every team handles everything itself, the same things get built again and again:

```text
Authentication
Authorization
Policy
Approval
Credentials
MCP integration
Retry
Rate limits
Budget control
Audit
Tracing
Kill switch
Deployment
Evaluation
```

And the organisation has no easy answer to questions such as:

```text
How many production agents do we have?

Who owns this agent?

Which agents can access customer data?

Which agents use this MCP server?

Which agents can create a Purchase Order?

If this MCP server is compromised, which agents are affected?

How much has the whole organisation spent on LLMs today?

Which agent keeps producing the same error?

How do we stop one agent version across the whole organisation?

Who approved this agent action?

Did this action actually happen?

If the API timed out, how do we know whether the work succeeded?
```

Enterprise Agent Control Plane is built to answer these problems.

---

# 3. Core Principle

The system splits responsibility across three planes.

```text
CONTROL PLANE

What exists?
Who owns it?
Where is it?
How is it configured?
What depends on what?
How is it deployed?
What is its health?

        │
        ▼

GOVERNANCE PLANE

Who is this?
Should this action be allowed?
What policy applies?
Does it require approval?
What restrictions apply?

        │
        ▼

EXECUTION PLANE

How do we execute it correctly?
Which worker runs it?
How do we avoid duplicate effects?
What happens on failure?
How do we recover?
```

## 3.1 Product Boundary — Rev 2

At first, EACP is an **Agent Tool / Action Control Plane**.

EACP sits in the path of **tool / action calls** that touch organisational resources, such as SAP, databases, GitHub, e-mail or MCP tools.

EACP is **not an LLM gateway** in the MVP.

```text
Agent ──LLM call──► LLM Provider             (not through EACP)
  │                      │
  │                      └─ usage/cost ─► EACP FinOps (ingest: OTel GenAI spans / provider billing)
  │
  └──tool/action──► EACP ──► Governance ──► Execution Fabric ──► Enterprise System
```

Reasons:

```text
An LLM gateway is a separate product with many competitors
latency-sensitive
It is not EACP's differentiator (§114)
```

The LLM gateway will be an optional module later (§97, ADR-001).

**The architecture must leave room for an LLM gateway now:**

```text
Ingress (per call type)            Shared core (reused by every ingress)
────────────────────────           ─────────────────────────────────────
Tool / Action API   (Slice A)  ─┐  Principal authentication
LLM Gateway         (later)    ─┼─► Capability check
A2A ingress         (later)    ─┘  GovernanceProvider
                                   Approval store
                                   Audit / evidence journal
                                   Budget (Slice B)
                                   Telemetry
```

The shared core must not be tied to the shape of a "tool call": a governance request uses a generic `operation` + `target` + `payload`,

so a new ingress can connect without changing the core.

## 3.2 Enforcement Point — Rev 2 (Slice A)

"Resources are accessed through the Control Plane" is only true when the agent **has no credential** for the privileged system and **no network path** to that system directly.

```text
Connector credentials  → only in the Execution Worker / an execution proxy that EACP controls;
                         never returned through the API, never in an action payload and never in a log
Agent                  → holds only an agent identity (a credential for calling the EACP API)
Capability             → an agent may call only the tools in the allowlist of its ACTIVE AgentVersion,
                         checked deterministically before governance, failing closed
Network                → the agent runtime has no egress to privileged systems
                         demo: docker network segmentation
                         production: network policy / egress firewall
```

Slice A uses only static secrets held by the worker.

JIT / short-lived credentials are Phase 24 (§96).

**The scope of the claim (Rev 2.1):** the claim "cannot bypass" holds for a **conforming deployment** (ADR-001 §3a),

that is, the target system issues privileged credentials only to the EACP worker/proxy, and the agent has no network route to the target.

**Evidence Slice A must be able to show:**

```text
agent calls the Fake ERP directly        → fails (no credential and no network path)
agent calls a tool outside its allowlist → DENIED before governance
search for a secret in API responses / logs / DB → none found
```

---

# 4. Relationship With Microsoft AGT

Microsoft Agent Governance Toolkit is the governance foundation.

AGT/ACS is used for things such as:

```text
Policy
Identity
Trust
Delegation
Approval
Runtime governance
Context governance
Protocol governance
Audit evidence
SRE primitives
```

Enterprise Agent Control Plane should not rebuild these without a reason.

**Rev 2 — a clear split of responsibilities:**

```text
AGT / ACS (sidecar PDP)          EACP (Go core)
──────────────────────           ──────────────────────────────────────
Policy evaluation / verdict      Approval STATE: request, vote, grant, consume
  (allow/warn/deny/escalate/     Approver eligibility / SoD enforcement
   transform)                    Decision evidence persistence
Action identity semantics        Execution, idempotency, lease/fencing
Audit primitives                 Durable audit journal
```

ACS is stateless, so it is **not a state store** for approvals (§5.1).

Architecture:

```text
                     Enterprise Agent Control Plane

┌──────────────────────────────────────────────────────────────┐
│                       CONTROL PLANE                          │
│                                                              │
│ Agent Registry      Dependency Graph      Fleet Management   │
│ Release Manager     Evaluation            Agent SOC          │
│ FinOps              Operator UI           API / CLI          │
└──────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────┐
│                     GOVERNANCE PLANE                         │
│                                                              │
│  GovernanceProvider (Go interface)                           │
│    ├── local provider        (Slice A)                       │
│    └── Microsoft AGT / ACS   (sidecar PDP, Slice B)          │
│                                                              │
│ Policy verdicts    Identity    Trust    Delegation           │
│ EACP-owned: Approval Store · Decision Evidence · SoD         │
└──────────────────────────────┬───────────────────────────────┘
                               │
                       Governance Decision
                               │
                               ▼
╔══════════════════════════════════════════════════════════════╗
║                      EXECUTION PLANE                         ║
║                                                              ║
║               Go Distributed Execution Fabric               ║
║                                                              ║
║ Admission           Fair Scheduler       Durable Queue       ║
║ Budget Reservation  Lease / Fencing      Idempotency         ║
║ Retry Safety        Backpressure          Reconciliation      ║
║ Connector Runtime   Worker Fleet         Execution Journal   ║
╚══════════════════════════════╤═══════════════════════════════╝
                               │
                               ▼
                 MCP / A2A / HTTP / gRPC
                               │
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
             SAP              DB              GitHub
```

---

# 5. Extension-First Strategy

By default, do not fork the whole Microsoft AGT repository.

Use:

```text
Microsoft AGT
      │
      │ public contracts
      ▼
Compatibility / Integration Layer
      │
      ▼
Enterprise Agent Control Plane
```

Reasons:

```text
AGT still evolves quickly
its API is still a Public Preview
ACS is still in development
the package layout may change
```

So AGT-specific code must be isolated.

For example:

```text
integrations/governance/microsoftagt/   (the sidecar's Go client)
sidecars/agt-pdp/                        (AGT Python/Rust sidecar)
```

The core domain must not depend on Microsoft-specific types directly.

## 5.1 Upstream Reality (verified 2026-09-23) — Rev 2

```text
AGT                     Public Preview v4.1.0, MIT, may break before GA
ACS                     in-process, stateless library (Rust core; SDK: Python/Node/.NET/Rust)
ACS Go binding          none
AGT Go SDK              core only (policy/identity/trust/audit)
                        no approval chains (issue #3083)
ACS verdicts            allow / warn / deny / escalate / transform
Action binding          JCS (RFC 8785) + SHA-256; approval binds to enforced identity
```

Consequences:

```text
Go calls ACS through a sidecar PDP (ADR-002)
EACP owns approval state (ADR-005)
Pin the AGT version + conformance tests in CI
```

---

# 6. Product Modules

The system is split into 10 modules.

```text
1. Agent Registry
2. Governance Integration
3. Distributed Execution Fabric
4. Tool / Connector Registry
5. Dependency & Blast Radius Graph
6. Agent Fleet Operations
7. Agent SRE / Observability
8. Agent FinOps
9. Release / Evaluation Platform
10. Agent Security Operations Center
```

---

# 7. Module 1 — Agent Registry

The Agent Registry is a CMDB for AI agents.

Every production agent must have a Passport.

For example:

```yaml
agent_id: procurement-agent
version: 14

owner:
  team: procurement-ai
  email: procurement-ai@example.com

environment: production

runtime:
  framework: langgraph
  language: python

models:
  - claude
  - gpt

tools:                     # Rev 2: this is the capability allowlist that is enforced
  - supplier_search
  - sap.create_po
  - email.send

data_classes:
  - internal
  - confidential

risk_class: high

governance:
  provider: microsoft-agt
  policy_set: procurement-production

lifecycle:
  status: active
  created_at: ...
  expires_at: ...
```

---

# 8. Agent Lifecycle

Agent state:

```text
DRAFT
  ↓
REGISTERED
  ↓
CERTIFIED
  ↓
STAGING
  ↓
CANARY
  ↓
ACTIVE
  ↓
DEPRECATED
  ↓
RETIRED
```

Alternative:

```text
SUSPENDED      (Rev 2: paused, and can be resumed)
QUARANTINED
REVOKED
```

Every transition must carry a reason and an audit event.

**Rev 2:** only an AgentVersion that is `ACTIVE` (and `CANARY` during a rollout) may submit actions.

This state is checked again at the release boundary (§15) and at the dispatch-intent commit (§23.1).

If it is SUSPENDED or QUARANTINED along the way, actions not yet dispatched are not executed.

---

# 9. Ownership Requirement

A production agent without an owner is a risk.

Policy:

```text
if environment == production
and owner == null

→ deployment denied
```

An owner can be:

```text
Human
Team
Service owner
Business unit
```

---

# 10. Agent Versioning

Separate:

```text
Agent
```

from:

```text
AgentVersion
```

For example:

```text
procurement-agent

v12
v13
v14
```

Versions may differ in:

```text
Prompt
Model
Tools
Policies
MCP dependency
Code hash
Evaluation score
Risk class
```

---

# 11. Agent Bill of Materials

Create:

> Agent BOM / Agent SBOM

For example:

```text
Procurement Agent v14

├── Code
│   └── git sha 92ca...
│
├── Model
│   └── claude-...
│
├── MCP
│   ├── sap-mcp v3
│   └── supplier-mcp v7
│
├── Skills
│   ├── supplier-analysis
│   └── purchase-order
│
├── Policies
│   └── procurement-prod@17
│
└── Data
    ├── supplier-db
    └── finance-api
```

Goals:

```text
Reproducibility
Security
Audit
Blast radius
Release comparison
```

---

# 12. Module 2 — Governance Integration

Microsoft AGT / ACS is the default governance provider,

but the Control Plane must not be locked in.

Interface concept (Rev 2 — ADR-002):

```go
type GovernanceProvider interface {
    // Evaluate is pure: no approval state lives in the provider.
    Evaluate(ctx context.Context, req GovernanceRequest) (GovernanceDecision, error)
}

type GovernanceDecision struct {
    Verdict         Verdict // Allow | Warn | Deny | Escalate | Transform
    EnforcedPayload []byte  // payload after transform (== input when no transform)
    InputDigest     Digest  // SHA-256(JCS(input binding))
    EnforcedDigest  Digest  // SHA-256(JCS(enforced binding))
    PolicyBundleID  string
    PolicyVersion   string
    ProviderID      string  // which PDP instance answered
    DecisionID      string
    Reasons         []string
    Approval        *ApprovalRequirement // set when Verdict == Escalate
    EvaluatedAt     time.Time
}
```

**Revalidation** is not an ACS primitive.

EACP defines revalidation itself as calling `Evaluate` again with the current snapshot before entering the atomic boundary (§15).

On an error, a timeout or an incomplete response → **fail closed** for new actions with side effects.

Rev 2.1: failing closed here means the action does not execute yet: it stays RECEIVED and the API answers 503 so it can be retried. It does not mean a permanent DENIED (ADR-002 §6).

But cancel, reconciliation reads and containment must never be blocked.

Providers:

```text
local            (Slice A — deterministic rules, versioned policy bundles)
MicrosoftAGT     (Slice B — sidecar PDP over HTTP; primary production integration)
OPA
Cedar
OpenFGA
Custom HTTP PDP
```

Microsoft AGT is the primary production integration,

but Slice A uses the `local` provider so that execution correctness does not have to wait for AGT.

---

# 13. Governance Context

The Control Plane stores:

```text
agent_id
agent_version
subject_id
tenant_id

governance_provider
provider_instance_id
policy_decision_id
policy_bundle_id
policy_version
verdict
reasons

input_digest
enforced_digest
enforced_payload

approval_request_id
approval_grant_id

trace_id
```

**Rev 2 — reference governance *decisions*, own approval *state*:**

EACP does not reimplement a policy engine,

but EACP **owns approval state** and **decision evidence**, because:

```text
ACS is stateless → there is no state to "duplicate"
an approval must be consumed in the same transaction as the action (§15)
audit must be able to reconstruct which policy version decided (invariant 10)
```

Without decision evidence → the action must not enter the atomic boundary (fail closed).

---

# 14. Action-Bound Approval

Use the ideas of the AGT approval protocol.

An action must be bound to:

```text
Agent
Subject
Operation
Target
Tool schema version
Resource
Parameters
```

Then create a canonical digest.

```text
ActionBinding
     ↓
RFC 8785 JCS
     ↓
SHA-256
     ↓
Action Digest
```

Change a parameter:

```text
2,400,000 THB
```

to:

```text
24,000,000 THB
```

and the old approval no longer applies.

## 14.1 Enforced Digest — Rev 2

If the policy answers `transform` (for example, redact a field or cap an amount), the payload that really executes is the **enforced payload**.

```text
input binding     → JCS → SHA-256 → input_digest      (request identity: idempotency conflict check, audit)
enforced binding  → JCS → SHA-256 → enforced_digest   (approval, revalidation and execution use this one)
```

A repeated idempotency key:

```text
same input_digest       → returns the original action
different input_digest  → 409 Conflict
```

The approver sees and approves the **enforced payload**.

The worker executes **only** the enforced payload persisted at the atomic boundary, never a payload taken directly from the agent.

## 14.2 Approval Model — Rev 2 (Slice A, ADR-005)

Approval state lives in EACP's Postgres:

```text
approval_requests   (tenant_id, action_id, enforced_digest, policy_version,
                     required_quorum, eligible_roles, expires_at, state)
approval_votes      (request_id, approver_principal_id, decision, reason,
                     authorization_basis, voted_at)
approval_grants     (request_id, action_id, enforced_digest, policy_version,
                     expires_at, consumed_at, consumed_by_action_id)
```

Rules (conservative):

```text
A grant is bound to tenant + action + enforced_digest + policy_version
A grant can be used once: it is consumed at the atomic boundary too
    UPDATE ... WHERE consumed_at IS NULL AND expires_at > now()
A grant can expire, and so can PENDING_APPROVAL (→ EXPIRED)
Separation of duties:
    approver ≠ the subject who asked
    approver ≠ the agent's owner
    approver is in the same tenant and holds the role the policy requires
Quorum: the number of votes the policy requires must be reached
    a single deny vote → DENIED (short-circuit)
The policy version changes before consumption → the old grant no longer applies
    → revalidate again
    → allow: continue (void the old request/grant) / escalate: ask for a new approval / deny: DENIED
Policy versions are immutable, with a tenant policy pointer that is locked together
    by activation and by release/dispatch (ADR-005 §5)
Policy / allowlist / contract activation is a two-person operation,
    and whoever authored or activated it must not approve actions that depend on it
Team owner: membership is checked in group_memberships; if it cannot be resolved → the vote is refused
Every vote and grant has an authorization basis and an audit event
```

Approvals are **durable**: they survive a restart because they live in Postgres, not in any process's memory.

---

# 15. Atomic Execution Boundary

This is the most important part of the Execution Plane.

After governance allows it, an action has still not started executing.

It must pass the atomic boundary.

**Rev 2 — the full sequence (ADR-005):**

```text
(0) Submission — its own transaction
    authenticate agent → capability check → claim idempotency key
    → persist action (RECEIVED, input payload, input_digest)

(1) Governance — outside any DB transaction, because no DB lock may be held while waiting for the PDP
    Evaluate → persist decision evidence
    → AUTHORIZED | PENDING_APPROVAL | DENIED

(2) Revalidation — outside the transaction, before release
    Evaluate again with the current snapshot
    → yields the latest policy_version + enforced_digest

(3) Release boundary — ONE TRANSACTION
    BEGIN
      lock action row (state ∈ {AUTHORIZED}), check not_after
      verify revalidation: policy_version matches the current policy version
                           enforced_digest matches the persisted one
      verify the agent version is still ACTIVE and the tool still in the allowlist
      consume the one-time approval grant (if any)
      reserve budget                       (a hook in Slice A; hard budgets in Slice B)
      transition AUTHORIZED → QUEUED
      append execution journal + audit event
      insert outbox event
    COMMIT
```

If any condition fails → ROLLBACK everything.

So the approval is not burned, and the action is not queued.

Only after COMMIT:

```text
Action is executable
```

Before COMMIT:

```text
No worker should execute the action
```

The actual execution still has to pass **fenced dispatch**, one more layer (§23).

---

# 16. Why Atomic Execution Boundary Matters

It prevents:

```text
An approval being used twice

Worker race

An action queued with no record in the DB

A budget overspent through a race

A duplicate request causing a duplicate external effect

An approval burned with no action behind it   (Rev 2)

A policy that changed while an old approval is still used    (Rev 2)
```

The atomic boundary does **not** prevent a duplicate external effect from a stale worker.

That case is solved by fenced dispatch (§23) and the reconciliation protocol (§20).

---

# 17. Module 3 — Distributed Execution Fabric

Go is the main language.

Responsibilities:

```text
Admission
Scheduling
Queueing
Worker Coordination
Leasing
Fencing
Idempotency
Retry
Backpressure
Reconciliation
Execution Evidence
```

---

# 18. Action State Machine

**Rev 2 — the full transition table is in ADR-004, which is the source of truth**

Happy path:

```text
RECEIVED
   ↓  governance: allow / warn / transform
AUTHORIZED
   ↓  release boundary (§15)
QUEUED
   ↓  worker claim: lease_generation++
LEASED
   ↓  fenced dispatch-intent commit (§23)
EXECUTING          ← means "may already have been dispatched", not "waiting"
   ↓  definitive success + evidence
SUCCEEDED
```

Approval path:

```text
RECEIVED ──escalate──► PENDING_APPROVAL ──grant (quorum)──► AUTHORIZED
                             │
                             ├── deny vote ──► DENIED
                             ├── timeout   ──► EXPIRED
                             └── cancel    ──► CANCELLED
```

Ambiguity path:

```text
EXECUTING ──timeout/5xx after send / worker crash / lease lost──► UNKNOWN_OUTCOME
UNKNOWN_OUTCOME ──reconciler lease──► RECONCILING
RECONCILING ──positive evidence──────────────────► SUCCEEDED
RECONCILING ──authoritative negative evidence────► RETRY_WAIT or FAILED
RECONCILING ──"not found" only / still unknown──► UNKNOWN_OUTCOME (backoff)
RECONCILING ──conflict / attempts exhausted─────► NEEDS_HUMAN_RESOLUTION
NEEDS_HUMAN_RESOLUTION ──operator resolution────► SUCCEEDED | FAILED | RETRY_WAIT (retry, same operation key)
```

Terminal:

```text
SUCCEEDED  FAILED  DENIED  CANCELLED  EXPIRED
```

Crash recovery (summary):

| State at the crash | Recovery |
|---|---|
| RECEIVED | The governance sweeper evaluates again (Evaluate is a pure function), or EXPIRED once past `not_after`. If the PDP is down it stays RECEIVED and the API answers 503 (it is not DENIED) |
| PENDING_APPROVAL / AUTHORIZED / QUEUED / RETRY_WAIT | Already in Postgres (durable), so work simply continues |
| LEASED (no dispatch intent yet) | Lease expires → back to QUEUED safely, because there is no side effect yet |
| EXECUTING | Lease expires → **UNKNOWN_OUTCOME**; never back to QUEUED. The one exception: a connector certified READ_ONLY or native-idempotent may be re-dispatched with the **same operation key** |
| RECONCILING | Reconciler lease expires → back to UNKNOWN_OUTCOME |

Changes from Rev 1:

```text
CREATED    → renamed RECEIVED (persisted before governance, so denied requests are audited)
ADMITTED   → merged into admission at submission (§26); a rejection is a 429 and creates no action
PREPARING  → merged into LEASED (worker-local)
DEAD_LETTER → a concept of messages/inboxes, not an action state;
              an action out of retries becomes FAILED(reason=retry_exhausted)
```

Every transition must have an actor, a reason and an audit event, as in §8.

---

# 19. First-Class UNKNOWN_OUTCOME

Never assume:

```text
timeout = failure
```

For example:

```text
Agent Control Plane
      │
      │ create PO
      ▼
     SAP

SAP creates PO

response lost
```

The Control Plane sees:

```text
timeout
```

But the truth may be:

```text
PO created
```

So:

```text
EXECUTING
    ↓
timeout after dispatch
    ↓
UNKNOWN_OUTCOME
```

Not:

```text
FAILED
```

**Rev 2 — UNKNOWN_OUTCOME arises in many ways, not only timeouts:**

```text
a timeout after the request was sent
a connection reset after the request was sent
a 5xx the connector has not certified as "no effect"
a worker crash / lease expiry during EXECUTING
a kill / cancel during EXECUTING
```

Only two kinds of result count as **definitive**:

```text
definitive success   = a successful response with an external reference
definitive no-effect = an error the connector contract certifies as having no side effect,
                       such as a declared validation 4xx or a connection refused before sending
```

Everything else is UNKNOWN_OUTCOME (the conservative default).

---

# 20. Reconciliation Engine

```text
UNKNOWN_OUTCOME
       ↓
Reconciliation Worker
       ↓
Check external world state
       ↓
```

Result:

```text
CONFIRMED_SUCCESS
CONFIRMED_NOT_EXECUTED
STILL_UNKNOWN
CONFLICT
```

Then:

```text
CONFIRMED_SUCCESS
→ SUCCEEDED

CONFIRMED_NOT_EXECUTED
→ retry may be allowed (only when the evidence meets the proof standard below)

STILL_UNKNOWN
→ retry reconciliation later → out of attempts/time → NEEDS_HUMAN_RESOLUTION

CONFLICT
→ NEEDS_HUMAN_RESOLUTION
```

## 20.1 Operation Identity — Rev 2

Every action has an **operation key** that stays the same for the action's whole life and does not change per attempt.

```text
operation_key = "eacp:{tenant_id}:{action_id}"
```

The connector must send the operation key to the external system:

```text
native idempotency     → as the external API's Idempotency-Key
correlation only       → embedded in a reference field of the record (e.g. the PO's external reference)
                         so that reconciliation can find it
none                   → can execute at most once only;
                         any ambiguity → NEEDS_HUMAN_RESOLUTION
```

## 20.2 Reconciliation Proof Standard — Rev 2

**"Not found" alone is not evidence that nothing executed.**

For example, SAP may index slowly, a lookup API may be eventually consistent, or the query may be wrong.

The connector contract (§31) must declare its proof standard:

| Evidence | Positive (success) | Negative (not executed) |
|---|---|---|
| `AUTHORITATIVE` | a record found by operation key | a lookup by operation key on a **strongly consistent** read path, and the connector certifies that "not found = never happened" |
| `BEST_EFFORT` | a record found by operation key | **not usable** → STILL_UNKNOWN |
| `NONE` | not usable | not usable → NEEDS_HUMAN_RESOLUTION |

Auto-retry rules after UNKNOWN_OUTCOME (conservative):

```text
READ_ONLY                                   → may retry
native idempotent (same operation key)      → may retry with the same key
irreversible / non-idempotent:
    AUTHORITATIVE negative evidence         → may retry with the same operation key
                                              if the retry policy allows
    everything else                         → NEEDS_HUMAN_RESOLUTION
```

## 20.3 Human Resolution — Rev 2

`NEEDS_HUMAN_RESOLUTION` is a state that a person must resolve explicitly.

An authenticated operator can:

```text
mark SUCCEEDED  (attaching the external reference / evidence)
mark FAILED     (attaching evidence that it did not execute)
authorize retry (with the same operation key; a high-risk action needs SoD)
```

Every resolution is a privileged action with a reason, an actor and an audit event (§57).

---

# 21. No Fake Exactly-Once

Never write marketing that says:

> Exactly-once execution across every external system

Use the terminology:

```text
Idempotent where supported

Effectively-once where reconcilable

At-most-once when retry is unsafe
```

---

# 22. Execution Lease

A worker must acquire a lease.

```text
action_id
worker_id
lease_generation
leased_until
heartbeat_at
```

Worker crash:

```text
heartbeat stops
 ↓
lease expires
 ↓
another worker may claim
```

**Rev 2 — claims read from Postgres only:**

```sql
SELECT ... FROM actions
WHERE state = 'QUEUED' AND tenant_id = ...
FOR UPDATE SKIP LOCKED
LIMIT n;
```

A worker **must never** execute from a message in a queue (NATS) directly. It must claim and fence through Postgres every time (§60).

Reclaiming an action whose lease expired:

```text
LEASED    (no dispatch intent)    → QUEUED           safe: no side effect yet
EXECUTING (with a dispatch intent) → UNKNOWN_OUTCOME  never re-dispatch,
                                    except READ_ONLY / native-idempotent with the same operation key
```

A worker must not start an external call if the lease it has left is shorter than `connector_timeout + safety_margin`,

and the call deadline must always be shorter than the lease left.

---

# 23. Fencing Token

Example:

```text
Worker A
generation = 10

A freezes

lease expires

Worker B
generation = 11

A wakes up
```

A must not commit

DB update:

```sql
UPDATE actions
SET state = 'SUCCEEDED'
WHERE id = $1
AND lease_generation = $2
AND state = 'EXECUTING';
```

generation 10's update fails

## 23.1 Fencing Must Cover Execution Semantics — Rev 2

Fencing in the DB **alone is not enough**.

An example failure in Rev 1:

```text
A (gen 10) sends create_po to SAP → freezes → its lease expires
B (gen 11) reclaims → sees state EXECUTING → sends create_po again
→ two POs
(A's commit is correctly rejected, but the side effect has already happened)
```

Rev 2 uses a **fenced dispatch protocol**:

```text
1. Dispatch intent (fenced, its own commit, before every external call)
     UPDATE actions SET state='EXECUTING', dispatch_intent_at=now()
     WHERE id=$1 AND lease_generation=$2 AND state='LEASED'
       AND leased_until > now() + $call_budget
     + checks in the same transaction (reading the registry FOR SHARE):
       AgentVersion ACTIVE, the tool still in the allowlist,
       the pinned connector contract still active and not revoked,
       tenant policy pointer == pinned policy_version (if not → back to AUTHORIZED)
     + INSERT action_attempts(attempt_no, lease_generation, worker_id,
                              operation_key, dispatched_at)
     if the update touches 0 rows → never call the external system

2. The external call uses the operation key (§20.1) and a deadline < the lease left,
   and sends the fencing generation for targets that support conditional writes

3. Result commit (fenced)
     ... WHERE lease_generation=$2 AND state='EXECUTING'

4. If a stale worker's commit fails
     → append "late result evidence" to the journal (no state change)
     → the reconciler uses it as evidence

5. Reclaiming an action with a dispatch intent → UNKNOWN_OUTCOME (§22); never re-dispatch
```

Outcome:

```text
DB state        fenced by the generation
External effect limited by the dispatch-intent rule + the operation key
                (idempotent/correlated at the external system) + reconciliation
```

---

# 24. Fair Scheduler

It must support many tenants/teams.

```text
Team A     10,000 jobs
Team B        100 jobs
Team C        100 jobs
```

A must not starve B/C

Slice B (Phase 12):

```text
Weighted Deficit Round Robin
+
Priority
+
Aging
```

**Rev 2:** Slice A uses a FIFO claim from Postgres plus a static per-tenant cap (§26).

The risk of starvation is accepted in Slice A, because its goal is correctness, not fairness.

The fair scheduler must choose the *claim order* in Postgres,

not depend on the order in which messages arrive in NATS (§60).

---

# 25. Scheduling Dimensions

```text
Tenant
Department
Team
Agent
Priority
Deadline
Connector
Cost
Concurrency
Side-effect risk
```

---

# 26. Backpressure

When capacity is full:

```text
Agent
 ↓
Admission
 ↓

ACCEPT

QUEUE

THROTTLE

REJECT
```

Never queue without a bound.

**Rev 2:** Slice A has static admission at submission.

```text
global max QUEUED
per-tenant max QUEUED
```

If it is exceeded → 429, and no action is created.

Fine-grained limits (tenant/connector/worker) are in Slice B (Phase 13).

**Phase 13 (ADR-022 §1):** adds a limit on unreleased actions per tenant and a queue limit per connector group (`max_queued`); a 429 names the `scope` of the limit that is full.

---

# 27. Bulkhead Isolation

Separate resource pools:

```text
SAP writes

GitHub writes

Email

LLM

Database
```

If SAP is down:

```text
SAP capacity exhausted
```

Email/GitHub must keep working

---

# 28. Circuit Breaker

```text
CLOSED
 ↓ failures
OPEN
 ↓ cooldown
HALF_OPEN
 ↓ probe success
CLOSED
```

scope:

```text
provider
connector
endpoint
tool
```

---

# 29. Retry Safety

A retry decision must rest on execution semantics.

```text
READ
→ retry

IDEMPOTENT WRITE
→ retry same key

REVERSIBLE WRITE
→ conditional retry

IRREVERSIBLE + NON-IDEMPOTENT
→ no blind retry
```

**Rev 2:**

A retry after a definitive no-effect follows the retry policy.

A retry after UNKNOWN_OUTCOME must always pass the rules of §20.2.

Every retry uses the **same operation key** (§20.1).

If the side-effect class is unknown → treat it as `IRREVERSIBLE_WRITE` + non-idempotent (at-most-once).

---

# 30. Retry Budget

There is:

```text
max_attempts
max_elapsed_time
max_retry_cost
```

to stop retry storms.

---

# 31. Module 4 — Tool / Connector Registry

Every external capability must be registered.

For example:

```yaml
connector: sap-production
tool: create_po

protocol: mcp

side_effect: [irreversible_write, financial]

idempotency:
  mode: native            # native | correlation_only | none
  key_field: Idempotency-Key

operation_identity:
  correlation_field: external_reference   # the operation key is embedded here

reconciliation:
  lookup: by_operation_key
  proof_standard: authoritative  # authoritative | best_effort | none
  consistency: strong

no_effect_errors:                # errors certified as having no side effect
  - http_400_validation
  - connection_refused_before_send

credentials:
  custody: worker         # the agent never receives the credential

concurrency:
  group: sap-write
  max_inflight: 20

data:
  sensitivity: confidential
```

**Rev 2 — the connector contract is operator-declared and certified:**

MCP/tool metadata is untrusted (§67).

So `side_effect`, `idempotency`, `reconciliation` and `no_effect_errors` must be declared by an operator in the registry.

What the server declares about itself (self-description) is supporting information only.

A contract is bound to the tool fingerprint (§33). If the fingerprint changes, the contract is invalid until it is recertified.

Without a contract → the tool cannot execute (fail closed).

---

# 32. Side-Effect Classification

```text
READ_ONLY

REVERSIBLE_WRITE

IRREVERSIBLE_WRITE

EXTERNAL_COMMUNICATION

FINANCIAL

ADMINISTRATIVE
```

One action may carry several tags.

---

# 33. Tool Fingerprint

Tool definitions must be fingerprinted.

```text
Tool Schema
 +
Metadata
 +
Security attributes
    ↓
Canonical form
    ↓
SHA-256
```

When an MCP server reconnects:

```text
old fingerprint
vs
new fingerprint
```

If it changed:

```text
Low risk
→ audit

High risk
→ quarantine / require recertification
```

---

# 34. Tool Certification

Lifecycle:

```text
DISCOVERED
   ↓
SCANNED
   ↓
VALIDATED
   ↓
CERTIFIED
   ↓
ACTIVE
```

Alternative:

```text
QUARANTINED
REVOKED
```

**Rev 2:** certifying a tool must include the **connector contract** (§31):

```text
side-effect class
idempotency mode
operation identity
reconciliation proof standard
no-effect errors
credential custody
```

The contract is bound to the fingerprint. If the fingerprint changes → the contract is invalid → the tool cannot execute until it is recertified.

---

# 35. Module 5 — Dependency Graph

Build a graph:

```text
Agent
 ↓
Model
 ↓
MCP Server
 ↓
Tool
 ↓
Application
 ↓
Dataset
```

Another example:

```text
Agent A
 ↓ delegates
Agent B
 ↓ uses
MCP X
 ↓ accesses
Database Y
```

---

# 36. Blast Radius

If:

```text
sap-mcp v3 compromised
```

The Control Plane can answer at once:

```text
Affected Agents: 41

Affected Teams:
Procurement
Finance
Operations

Affected Workflows: 17

Data classes:
Internal
Confidential

Production runs in last 24h:
1,284
```

---

# 37. Initial Graph Storage

MVP:

```text
PostgreSQL
+
recursive CTE
```

Do not use a graph database until there is a real reason.

Later it may become:

```text
Neo4j
Memgraph
etc.
```

once the graph's complexity justifies it.

---

# 38. Module 6 — Fleet Operations

See agents as a fleet.

An operator must be able to:

```text
list
filter
inspect
pause
resume
kill
quarantine
roll back
upgrade
```

Agent fleet dashboard:

```text
Total Agents      1,270
Production          421
Canary               18
Quarantined           3
Unknown Owner         7
Policy Drift          5
```

---

# 39. Distributed Kill Switch

Use AGT's kill semantics,

but the Control Plane does the distributed propagation.

scope:

```text
Global
Tenant
Team
Agent
Agent Version
Run
Action
Connector
Tool
Model
```

---

# 40. Kill Propagation

```text
Operator
 ↓
Control Plane
 ↓
Postgres authoritative kill state
 ↓
NATS event
 ↓
Workers / gateways
```

Worker:

```text
check before lease

check before execute

listen for live kill event

cancel context if possible
```

But:

```text
cancel != external rollback
```

So a dispatched action may need reconciliation.

**Rev 2 (Slice C, Phase 16) — closing the TOCTOU:**

```text
kill_epoch        Postgres keeps a monotonic kill_epoch per scope
                  as the authoritative state
Fenced check      the kill state is checked in the same transaction as the dispatch-intent commit (§23.1);
                  if killed, the commit fails and there is no external call
Backstop          the worker polls kill_epoch periodically, because a NATS event may be lost or late
EXECUTING         a kill during EXECUTING → cancel the context → UNKNOWN_OUTCOME → reconcile;
                  it is not treated as FAILED
Containment       kill / reconcile / cancel must work even when the governance PDP is down
```

Slice A has only the agent `SUSPENDED` lifecycle, checked at the release boundary and the dispatch-intent commit.

The full distributed kill switch is in Slice C.

---

# 41. Module 7 — Agent SRE & Observability

Use AGT SRE + OpenTelemetry.

The Control Plane adds a fleet-level view.

Trace:

```text
User Request
  ↓
Agent Run
  ↓
Governance Evaluation
  ↓
Approval
  ↓
Admission
  ↓
Queue Wait
  ↓
Worker Lease
  ↓
Tool Execution
  ↓
Reconciliation
  ↓
Outcome
```

---

# 42. Execution Evidence vs Governance Evidence

A clear split:

```text
Governance Evidence

Who?
What policy?
What approval?
Why allow?
```

from:

```text
Execution Evidence

Which worker?
When dispatched?
Which idempotency key?
Which external transaction?
What happened?
Was reconciliation required?
```

Linked by:

```text
action_id
decision_id
trace_id
```

**Rev 2 — Evidence journal (Slice A):**

```text
append-only       no UPDATE / DELETE (enforced by the DB role)
hash-chained      each event has a prev_hash per tenant (tamper-evident)
coverage          every state transition, decision evidence, approval vote/grant/consume,
                  dispatch intent, late result evidence, reconciliation evidence,
                  human resolution
```

Governance evidence must record:

```text
provider
provider_instance_id
policy_bundle_id
policy_version
verdict
reasons
input_digest
enforced_digest
evaluated_at
```

Execution evidence must record:

```text
worker_id
lease_generation
operation_key
dispatched_at
external_reference
outcome
```

The OTel trace context (W3C `traceparent`) must be carried through the action row, the outbox and message headers,

so that a trace does not break at the queue.

---

# 43. Outcome Attestation Bridge

The Execution Fabric produces a verified outcome:

```text
CONFIRMED_SUCCESS
CONFIRMED_FAILURE
UNKNOWN
CONFLICT
```

and bridges it back to the Governance/Audit plane.

```text
Execution Evidence
       ↓
Outcome Attestation
       ↓
AGT / Audit / SIEM
```

---

# 44. Key Metrics

```text
actions_received_total

actions_authorized_total

actions_denied_total

actions_executed_total

actions_unknown_total

actions_reconciled_total

queue_depth

queue_wait_seconds

active_workers

lease_expiration_total

stale_commit_rejected_total

retry_total

connector_error_rate

budget_reserved

budget_committed

governance_latency

execution_overhead

approvals_pending                (Rev 2)

approval_grants_consumed_total   (Rev 2)

dispatch_intent_total            (Rev 2)

needs_human_resolution           (Rev 2)

late_result_evidence_total       (Rev 2)
```

**Rev 2 — label cardinality:** metric labels may only be bounded dimensions, such as tenant, connector, state, verdict.

Never use `agent_id` or `action_id` as a label.

To look at a single agent, use traces, logs or a Postgres query.

---

# 45. Module 8 — Agent FinOps

Including:

```text
Cost observation
Budget
Reservation
Quota
Forecast
Chargeback
```

**Rev 2 — Scope:**

Hard budget reservation for tool actions is in Slice B (Phase 11).

Full FinOps comes after Slice C (Phase 18).

EACP is not in the LLM path (§3.1), so LLM cost comes from **ingest**, not from proxying:

```text
OTel GenAI spans from the agent runtime
provider billing / usage export
```

If an LLM gateway module is added later, it can reserve budget per LLM call.

**Delivered (Phase 25b, 2026-09-27, ADR-031):** the LLM gateway reserves a hard budget per LLM call at PostgreSQL's estimate, commits the priced usage and records it as `gateway` usage beside OTel.

---

# 46. Hierarchical Budget

```text
Organization
      ↓
Business Unit
      ↓
Department
      ↓
Team
      ↓
Agent
      ↓
Run
```

resources:

```text
Money
Tokens
Model calls
Tool calls
GPU seconds
API units
Concurrent workers
```

---

# 47. Strict Budget Reservation

```text
RESERVE estimate
       ↓
EXECUTE
       ↓
OBSERVE actual
       ↓
COMMIT actual
       ↓
RELEASE remainder
```

For example:

```text
Remaining = $10

Agent A asks $7
Agent B asks $7
```

atomic reservation:

```text
A gets $7

remaining = $3

B denied/waits
```

It prevents concurrent overspending.

**Rev 2 — contention and leaks:**

```text
Lock order        always lock budget accounts from root → leaf (prevents deadlock)
Escrow            a parent allocates quota to a child in advance;
                  the hot path locks only the leaf row, not the Organization row every time
Reservation TTL   every reservation has expires_at;
                  a sweeper releases reservations left over from crashed flows
Hard vs soft      only hard budgets are in the atomic boundary;
                  soft budgets are computed asynchronously
UNKNOWN_OUTCOME   the reservation of an action with an unclear outcome is not released until it is reconciled
                  (conservative)
```

---

# 48. Chargeback

Show:

```text
Engineering Team

LLM            $8,241
Search APIs      $781
MCP services     $210

Total          $9,232
```

Aggregate cost per:

```text
Agent
Team
Business unit
Workflow
User
Customer
```

---

# 49. Module 9 — Release & Evaluation Platform

A production agent deployment must go through a pipeline.

```text
Code
 ↓
Unit tests
 ↓
Policy tests
 ↓
Security tests
 ↓
Agent evaluations
 ↓
Replay
 ↓
Shadow
 ↓
Canary
 ↓
Production
```

---

# 50. Agent Certification Pipeline

```text
REGISTER
 ↓
BUILD
 ↓
STATIC CHECK
 ↓
POLICY TEST
 ↓
SECURITY SCAN
 ↓
EVALUATION
 ↓
HUMAN REVIEW
 ↓
CERTIFIED
```

---

# 51. Replay

Take old production traces:

```text
Agent v14
```

and run them against:

```text
Agent v15
```

Compare:

```text
Policy decisions

Tool selection

Cost

Latency

Result quality

Risk

Errors
```

**Rev 2 — Research-grade:**

LLM agents are not deterministic.

So replay needs a record/replay connector that answers with the recorded tool responses.

Comparisons must look at distributions, not at every result matching.

---

# 52. Shadow Deployment

```text
Production request
       │
       ├─────────────► v14 real execution
       │
       └─────────────► v15 shadow
```

v15:

```text
cannot perform destructive effects
```

Keep only decision/result comparisons.

**Rev 2:** "no destructive effect" must hold **structurally**.

Shadow uses a connector with no write credential and no network path to the real system (§3.2).

Relying on a flag alone is not enough.

---

# 53. Canary

Deployment progression:

```text
1%
 ↓
5%
 ↓
25%
 ↓
50%
 ↓
100%
```

Automatic rollback if:

```text
SLO drops
policy violations increase
unknown outcomes rise
cost spikes
latency spikes
```

---

# 54. Module 10 — Agent Security Operations Center

The operator UI brings together:

```text
Inventory
Alerts
Policy violations
Approvals
Active incidents
Agent health
Tool risk
MCP drift
Kill switches
Cost anomalies
Unknown outcomes
```

---

# 55. Agent SOC Dashboard

For example:

```text
Enterprise Agent Security

Agents
────────────────────────
Registered            1,270
Production              421
High Risk                82
Quarantined               3

Security
────────────────────────
Blocked Actions        4,812
Approval Pending          17
MCP Drift                  5
Policy Drift               3

Execution
────────────────────────
Running                   812
Queued                  3,428
Unknown Outcome             2

FinOps
────────────────────────
Spend Today          $12,421
Budget Warnings            11
Hard Blocks                 2
```

---

# 56. Incident View

One incident:

```text
MCP schema changed unexpectedly
```

The Control Plane shows:

```text
MCP:
sap-production

Old fingerprint:
abc...

New fingerprint:
xyz...

Affected agents:
41

Active runs:
13

Data sensitivity:
Confidential

Recommended containment:
Disable MCP
Pause affected agents
Re-run certification
```

The operator makes the decision.

---

# 57. Human-In-The-Loop

Human intervention is used for:

```text
High-risk approval
Unknown outcome
Security incident
Policy exception
Conflict resolution
Production promotion
Break-glass
```

AI may help:

```text
summarize
classify
recommend
explain
```

but authoritative control decisions should be deterministic, or an authenticated human decision as the policy requires.

**Rev 2 — privileged control-plane actions must be governed too:**

Operator/admin actions that change safety state:

```text
resolve NEEDS_HUMAN_RESOLUTION
approve / deny
editing a connector contract
editing an AgentVersion's allowlist
changing a policy bundle
lifting a kill
editing a budget
```

Everything in this list must have:

```text
authenticated principal + role
a reason (mandatory)
an audit event in the hash-chained journal
separation of duties for high-risk actions
break-glass: allowed, but it requires a post-incident review
```

---

# 58. Core Domain Entities

```text
Tenant

Organization
Department
Team

HumanPrincipal
ServicePrincipal

Agent
AgentVersion
AgentInstance

Run
Action
ActionAttempt

Tool
ToolVersion
Connector

Model
ModelProvider

PolicyBundle            (Rev 2: versioned, for the local provider)
GovernanceDecision      (Rev 2: evidence is persisted, not only referenced)

ApprovalRequest         (Rev 2: EACP-owned)
ApprovalVote
ApprovalGrant

AgentCredential         (Rev 2: agent → EACP identity only)
ConnectorContract       (Rev 2: operator-declared, bound to the fingerprint)
ConnectorSecretRef      (Rev 2: refers to a secret the worker holds; never stores the secret value)

BudgetAccount
BudgetReservation
UsageRecord

Lease

ExecutionJournal
AuditEvent              (Rev 2: append-only, hash-chained)
HumanResolution         (Rev 2)

DependencyEdge

Release
Evaluation
Deployment

Incident
KillSwitch

AuditReference
```

---

# 59. Storage Architecture

Primary:

```text
PostgreSQL
```

Use for:

```text
Registry
Actions
Leases
Idempotency
Approvals          (Rev 2)
Decision evidence  (Rev 2)
Audit journal      (Rev 2)
Budgets
Outbox
Dependencies
Deployments
Kill state
Incidents
```

**Rev 2:** Postgres is the **single source of truth** for execution state (ADR-014).

Every critical table has `tenant_id` and is under Row-Level Security (§69).

---

# 60. Messaging

Use:

```text
NATS JetStream
```

for:

```text
work-available hints   (Rev 2: formerly "action dispatch")
execution events
kill propagation
registry updates
health events
reconciliation wake-ups
```

Assume:

```text
messages may duplicate
messages may be lost or delayed   (Rev 2)
```

**Rev 2 — NATS is not an execution authority:**

```text
A NATS message = a hint carrying only the action_id
The worker must always claim + fence through Postgres (§22)
Without NATS → the system is still correct, only slower (the worker polls Postgres)
```

Slice A does not use NATS: the worker polls, or uses Postgres `LISTEN/NOTIFY`.

NATS JetStream arrives in Slice B (Phase 10).

---

# 61. Cache

Redis optional

Use for:

```text
hot registry cache
policy reference cache
rate limits where appropriate
dashboard acceleration
```

Redis is not a source of truth for correctness-critical execution state.

---

# 62. Transactional Outbox

```sql
BEGIN;

UPDATE action;        -- state transition (Rev 2: e.g. AUTHORIZED → QUEUED)

INSERT outbox_event;

COMMIT;
```

Outbox publisher:

```text
Postgres
 ↓
NATS            (Slice B; Slice A: outbox → log sink / LISTEN-NOTIFY)
```

Outbox events must carry the trace context (`traceparent`).

It prevents:

```text
DB committed
but process crashed before publish
```

---

# 63. Inbox / Dedup

The consumer stores:

```text
message_id
consumer
processed_at
```

duplicate:

```text
ACK
do not execute again
```

---

# 64. API Layer

External:

```text
REST / JSON
```

High throughput internal:

```text
gRPC
```

Events:

```text
NATS
```

---

# 65. Example APIs

```text
POST /v1/agents

GET /v1/agents/{id}

POST /v1/actions                        (Rev 2: requires an Idempotency-Key; supports ?wait=<duration>)

GET /v1/actions/{id}

GET /v1/actions/{id}/events             (Rev 2: SSE for long-running actions)

POST /v1/actions/{id}/cancel

POST /v1/actions/{id}/reconcile

POST /v1/actions/{id}/resolve           (Rev 2: human resolution; operators only)

GET /v1/approvals?state=pending         (Rev 2)

POST /v1/approvals/{id}/votes           (Rev 2: approvers only; SoD is checked)

GET /v1/dependencies/blast-radius

POST /v1/killswitch

GET /v1/incidents

GET /v1/budgets

GET /v1/fleet/health
```

**Rev 2 — Synchronous result path:**

Most agent frameworks want a tool call's result immediately.

```text
POST /v1/actions?wait=30s
  → 200 + result           if it finishes (terminal) within the wait
  → 202 + action_id + state if it has not, e.g. PENDING_APPROVAL, QUEUED, UNKNOWN_OUTCOME
```

The client follows up with `GET /v1/actions/{id}` or SSE.

The agent SDK/adapter must turn a `202` into a result the agent understands, such as "pending approval", without retrying by itself.

The control-plane overhead SLO will be set after real measurement (§105).

---

# 66. CLI

```text
eacp agent list

eacp agent inspect procurement-agent

eacp action inspect act_123

eacp action reconcile act_123

eacp action resolve act_123 --outcome succeeded --evidence PO-9822 --reason "..."   (Rev 2)

eacp approval list --pending                                                     (Rev 2)

eacp approval vote apr_456 --approve --reason "..."                              (Rev 2)

eacp dependency blast-radius sap-mcp

eacp kill agent procurement-agent:v14

eacp connector disable sap-production

eacp budget inspect procurement

eacp fleet status
```

---

# 67. Security Architecture

Zero trust assumption:

```text
Agent is untrusted

LLM output is untrusted

Tool metadata is untrusted

MCP server is untrusted

Queue input is untrusted

External API output is untrusted
```

---

# 68. Threat Model

At minimum it must cover:

```text
Prompt injection

Tool poisoning

MCP rug pull

Agent impersonation

Confused deputy

Privilege escalation

Approval replay

Parameter substitution

Duplicate execution

Credential theft

Cross-tenant access

Budget race

Denial of wallet

Retry storm

Queue flooding

Compromised worker

Compromised connector

Audit manipulation

Supply-chain compromise

Unknown-outcome mishandling

Control-plane bypass (the agent holds a credential / has a direct network path)   (Rev 2)

Stale-worker duplicate dispatch                                    (Rev 2)

False-negative reconciliation ("not found" → retry → duplicate)     (Rev 2)

Approval self-approval / cross-tenant approver                     (Rev 2)

A stale approval after the policy changed                           (Rev 2)

Forged / replayed governance decision (PDP)                        (Rev 2)

Malicious operator / admin                                         (Rev 2)

Sensitive data in traces / audit (PII, secrets)                     (Rev 2)
```

Details and mitigations are in `docs/security/THREAT_MODEL.md` (Phase 0).

---

# 69. Tenant Isolation

Every critical record has:

```text
tenant_id
```

Tests:

```text
Tenant A cannot:

read Tenant B agent

consume Tenant B budget

reuse Tenant B idempotency key

kill Tenant B agent

read Tenant B trace

approve Tenant B action          (Rev 2)

consume Tenant B approval grant  (Rev 2)
```

**Rev 2 — Mechanism (Slice A, ADR-021):**

```text
Postgres Row-Level Security on every critical table
each transaction sets SET LOCAL app.tenant_id
the application role has no BYPASSRLS
every unique key includes tenant_id:
    (tenant_id, agent_id, idempotency_key)
    (tenant_id, operation_key)
    ...
Tenant isolation tests must run against real RLS, not only application checks.
```

Data handling:

```text
traces / logs / audit must never contain a secret
sensitive payloads are redacted by data class before leaving Postgres
```

Data residency and legal hold are in §113.

---

# 70. Credential Architecture

**Rev 2 — credential custody is a Slice A requirement, not an optimisation:**

> Agents must hold no credential for calling a privileged external system directly.

(It used to say "avoid sending static secrets into the agent directly where possible", which was too weak.)

Slice A:

```text
The worker holds the static connector secret (env / mounted file)
ConnectorSecretRef in the DB stores only a reference, never the value
No API returns a secret
Secrets are never in action payloads, the journal, traces or logs (with redaction tests)
The agent gets only an agent credential for calling the EACP API
```

Phase 24 (JIT credentials):

```text
Agent
 ↓
Action
 ↓
Execution Worker
 ↓
Credential Provider
 ↓
short-lived credential
 ↓
External Resource
```

Providers:

```text
AGT Credential System
SPIFFE/SPIRE
Azure Workload Identity
AWS IAM
Vault
Custom enterprise secret broker
```

---

# 71. Repository Structure

```text
enterprise-agent-control-plane/
│
├── cmd/
│   ├── controlplane-api/
│   ├── execution-worker/
│   ├── scheduler/          (Slice B)
│   ├── event-worker/       (Slice B)
│   ├── fakeerp/            (Slice A demo target)
│   └── eacpctl/
│
├── sidecars/
│   └── agt-pdp/            (Slice B: AGT/ACS sidecar, Python)
│
├── internal/
│   ├── registry/
│   ├── lifecycle/
│   ├── identity/           (Rev 2: agent / operator principals)
│   ├── capability/         (Rev 2: tool allowlist enforcement)
│   ├── governance/
│   ├── approval/           (Rev 2: EACP-owned approval store)
│   ├── digest/             (Rev 2: JCS + SHA-256)
│   ├── audit/              (Rev 2: hash-chained journal)
│   ├── action/
│   ├── admission/
│   ├── scheduler/
│   ├── lease/
│   ├── idempotency/
│   ├── reconciliation/
│   ├── connector/
│   ├── dependency/
│   ├── fleet/
│   ├── killswitch/
│   ├── budget/
│   ├── release/
│   ├── evaluation/
│   ├── incident/
│   ├── telemetry/
│   ├── outbox/
│   ├── inbox/
│   └── storage/
│
├── integrations/
│   ├── governance/
│   │   ├── microsoftagt/
│   │   ├── opa/
│   │   └── local/
│   │
│   ├── connectors/
│   │   ├── http/
│   │   ├── mcp/
│   │   ├── a2a/
│   │   └── fakeerp/
│   │
│   └── credentials/
│
├── api/
│   ├── proto/
│   └── openapi/
│
├── web/
│
├── migrations/
│
├── deployments/
│   ├── docker/
│   ├── helm/
│   └── kubernetes/
│
├── examples/
│   ├── procurement/
│   ├── finance/
│   └── coding-agent/
│
├── test/
│   ├── integration/
│   ├── concurrency/
│   ├── security/
│   ├── chaos/
│   └── load/
│
├── research/
│   └── REFERENCES.md
│
├── docs/
│   ├── architecture/
│   ├── adr/
│   ├── security/
│   ├── benchmarks/
│   └── MASTER_PLAN.md
│
├── AGENTS.md
├── CLAUDE.md
├── THIRD_PARTY_NOTICES.md
├── docker-compose.yml
└── README.md
```

---

# 72. Service Boundary

Do not split into microservices too early.

Start with (Rev 2 — Slice A):

```text
Control Plane API
Worker
Postgres
Fake ERP        (the demo target, on a different network from the agent)
```

Slice B adds:

```text
Scheduler
NATS
AGT sidecar PDP
```

Split further once scale justifies it:

```text
Registry Service

FinOps Service

Dependency Service

Incident Service

Release Service
```

---

# 73. Phase 0 — Research & Architecture

Before writing production code:

Study:

```text
Microsoft Agent Governance Toolkit   (pin the version; confirm the Python SDK / ACS API the sidecar uses)
Agent Control Specification
Temporal
River
NATS JetStream
OpenTelemetry Collector
Kubernetes scheduler concepts
SPIFFE/SPIRE
RFC 8785 (JCS) Go implementations
PostgreSQL Row-Level Security
```

Create:

```text
research/REFERENCES.md

docs/architecture/system-boundary.md
docs/architecture/control-plane.md
docs/architecture/governance-plane.md
docs/architecture/execution-plane.md
docs/architecture/failure-model.md
docs/security/THREAT_MODEL.md
```

**Rev 2 — Phase 0 gate:**

Do not start Phase 1 until ADR-001, ADR-002, ADR-004 and ADR-005 are **Accepted**.

ADR-004 (the state machine) is a hard gate, because it is the correctness boundary for approval, retry, reconciliation and audit.

---

# 74. Required ADRs

```text
ADR-001 Product Boundary & Enforcement Point         (Phase 0 gate)

ADR-002 AGT Integration Strategy (Sidecar PDP)        (Phase 0 gate)

ADR-003 Agent Registry Model

ADR-004 Action State Machine & Execution Semantics   (Phase 0 gate)

ADR-005 Approval Ownership & Atomic Execution Boundary (Phase 0 gate)

ADR-006 Delivery Semantics

ADR-007 Lease and Fencing

ADR-008 Idempotency

ADR-009 Unknown Outcome

ADR-010 Reconciliation

ADR-011 Scheduler Fairness

ADR-012 Budget Reservation

ADR-013 Connector Contract

ADR-014 PostgreSQL as Execution Authority; NATS for Signals

ADR-015 Dependency Graph

ADR-016 Distributed Kill Switch

ADR-017 Outcome Attestation

ADR-018 Release & Evaluation

ADR-019 Credential Custody                            (Rev 2)

ADR-020 Build vs Adopt Execution Engine (Temporal / River)  (Rev 2)

ADR-021 Tenant Isolation & Data Handling               (Rev 2)
```

ADR-007 to ADR-010 must agree with ADR-004, which is the one that decides.

---

# 75. Phase 1 — Platform Foundation (Slice A)

**Rev 2 — Delivery slices:**

```text
Slice A — Correct, non-bypassable execution     Phase 1–8
Slice B — Enterprise governance & capacity       Phase 9–13
Slice C — Fleet safety                           Phase 14–17
Later   — Operations & ecosystem                 Phase 18–25
```

Each slice ends with a demo that uses **only capabilities that exist in that slice or earlier**.

Build:

```text
Go module + services (controlplane-api, execution-worker)
PostgreSQL + migration system
RLS scaffolding (tenant_id + policies from the first migration)
configuration
structured logging (slog) + secret redaction
OpenTelemetry
health endpoints
graceful shutdown
docker-compose: postgres, api, worker, fakeerp
    separate networks: the agent network has no route to fakeerp
```

No NATS yet in Slice A (§60)

---

# 76. Phase 2 — Registry, Identity & Capability (Slice A)

Build:

```text
Agent
AgentVersion
Owner
Environment
Risk class
Lifecycle (at least REGISTERED / ACTIVE / SUSPENDED / RETIRED)
Tool allowlist per AgentVersion

Agent identity / credential for calling EACP (Slice A: a per-agent API key, hashed at rest)
Operator / approver principals + roles

Connector registry + operator-declared ConnectorContract (§31)
ConnectorSecretRef (the secret is held by the worker only)
```

Enforce:

```text
a production agent without an owner → cannot be registered/activated (§9)
capability check: the tool must be in the ACTIVE AgentVersion's allowlist
    → if not, DENIED before governance
a tool with no ConnectorContract → cannot execute
```

CLI:

```text
agent register
agent list
agent inspect
connector register
```

**Status (2026-09-23): delivered.** The normative detail is in [ADR-003](adr/ADR-003-agent-registry-identity-and-capability.md) (Rev 1.1), which is stricter than this section in several places:

- An owner is required in **every** environment, not only production.
- Two-person activation applies to allowlists, contracts and agent versions.
- Role grants and credential registration are two-person too.
- Keys are bring-your-own-key and bound to one agent version.
- Contracts are fingerprinted, and a tool with no idempotency can't be retried automatically.
- The audit journal lands in this phase, not Phase 4.

---

# 77. Phase 3 — Governance Interface, Local Provider & Approval Store (Slice A)

Implement:

```text
GovernanceProvider interface (ADR-002)
local provider: deterministic rules from a versioned policy bundle
    verdicts: allow / warn / deny / escalate / transform
digest: JCS (RFC 8785) + SHA-256, input_digest + enforced_digest
decision evidence persistence
approval store (ADR-005): request, vote, grant, consume
approver eligibility + separation of duties + quorum + expiry
policy-version binding of grants
```

Tests:

```text
approval replay (consumed twice) → the second fails
parameter substitution → the digest does not match → cannot be used
transform-then-approve → bound to the enforced payload
self-approval / cross-tenant approval → refused
the policy changes after the grant → the grant cannot be used
PDP error → fail closed
```

The AGT sidecar is **not built yet** in this phase; it comes in Phase 9.

**Status (2026-09-23): delivered.** `internal/governance` implements the local provider and binding digests. Migration 00004 and `internal/approval` implement policy versions, evidence, requests, votes and grants under tenant RLS. The API exposes policy administration and approver queue/voting. Phase 4 still owns action creation, atomic release and worker execution. The Phase 3 review is in `docs/reviews/2026-09-23-phase3-code-review.md`.

---

# 78. Phase 4 — Action API & Atomic Boundary (Slice A)

Implement:

```text
POST /v1/actions (Idempotency-Key, ?wait=)
The action state machine per ADR-004
Idempotency (tenant, agent, key) + 409 when the input_digest differs
Release boundary (§15): revalidate → consume grant → QUEUED → journal → outbox
Budget reservation hook (a no-op in Slice A)
Static admission limits (§26)
Hash-chained audit journal
Outbox table
```

**Status (2026-09-23): delivered.** Migration 00005 and `internal/action` implement T1–T13 and T15 of ADR-004:
- Idempotent submission, with 409 when the digest differs.
- Static admission, with 429 and no action created.
- Governance evaluation with 503 on T2a.
- Durable approval cascades.
- The ADR-005 release boundary: fresh revalidation, one-time grant consumption checked at commit, pinned policy and contract, the budget hook, the journal and an outbox row.
- Pre-dispatch cancel and expiry.

PostgreSQL triggers enforce every guard for raw SQL as `eacp_app`. The API adds `POST /v1/actions`, `GET /v1/actions/{id}` and `POST /v1/actions/{id}/cancel`, and `controlplane-api` runs the sweeper. Implementation choices are recorded in ADR-004 Rev 2.2, ADR-005 §9 and ADR-002 Rev 2.3. Workers, leases and dispatch are Phase 5. The review is in `docs/reviews/2026-09-23-phase4-code-review.md`.

---

# 79. Phase 5 — Worker, Lease, Fencing & Dispatch Intent (Slice A)

Implement:

```text
claim from Postgres (FOR UPDATE SKIP LOCKED, FIFO)
heartbeat
lease expiry / reclaim per the rules of §22
generation fencing on every DB write
a fenced dispatch-intent commit before the external call (§23.1)
late result evidence
credential custody: the worker loads the connector secret; the agent has none
```

Mandatory concurrency tests:

```text
lease race
a stale worker's commit is rejected
a stale worker never causes a second dispatch (non-idempotent)
```

**Status (2026-09-23): delivered.** Migration 00006 and `internal/worker` implement ADR-004 T14 and T16–T27:
- Claim: a FIFO hint filtered by protocol and credential, then `FOR UPDATE SKIP LOCKED` plus a state CAS that takes the next lease generation.
- Heartbeats that extend only a live lease and carry cancel requests.
- A fenced dispatch intent that inserts the attempt row in the same transaction and re-checks drift (T16a/T16b).
- Fenced result commits with conservative classification.
- Late-result evidence.
- Sweeper reclaim (T17, T23, T24 narrowed to READ_ONLY) and retry scheduling (T25–T27).
- Cancel requests in `EXECUTING` and `RETRY_WAIT`.

PostgreSQL rejects every write by a worker that doesn't hold the lease at the named generation, including raw SQL as `eacp_app`.

Connector credentials are tenant-namespaced and host-bound, and only `execution-worker` may load them. Compose mounts them into the worker only, and a secret canary never reaches rows, attempts, the journal, the outbox or logs.

The three mandatory concurrency tests are in `internal/worker/worker_test.go`: the lease race, a stale commit rejected, and a stale worker never dispatching twice. Implementation choices are recorded in ADR-004 Rev 2.3. At Phase 5 completion no connector protocol was registered; Phase 6 adds HTTP and Fake ERP, while reconciliation remains Phase 7. The review is in `docs/reviews/2026-09-23-phase5-code-review.md`.

---

# 80. Phase 6 — Connector Framework & Fake ERP (Slice A)

**Implementation status (2026-09-23): delivered.** The worker registers the HTTP connector, whose execute and lookup methods use the operation key under the pinned contract. Fake ERP implements credential-protected purchase order calls, a durable operation-key lookup, an audit log, and the failure modes below. The strong `create_po` tool and eventual `create_po_eventual` tool have distinct proof standards (ADR-004 Rev 2.4). PostgreSQL integration tests cover success, response loss, `UNKNOWN_OUTCOME`, and the absence of automatic re-dispatch. Compose mounts the ERP credential only into the worker and Fake ERP, and keeps the ERP operation log in a durable volume. Network-isolation and unauthenticated-call tests pass. The review is in `docs/reviews/2026-09-23-phase6-code-review.md`.

Build:

```text
Connector interface: Execute(operation_key, enforced_payload) / Lookup(operation_key)
HTTP connector
Fake ERP (requires a credential; knows operation keys; has a lookup API)
```

Fake ERP supports:

```text
success
fail before execute
execute then timeout
execute then connection reset
slow response
429
5xx before effect
5xx after effect
outage
delayed visibility (the record exists but a lookup does not find it yet)
```

---

# 81. Phase 7 — UNKNOWN_OUTCOME, Reconciliation & Human Resolution (Slice A)

**Implementation status (2026-09-23): delivered.** Migration 00007 adds T28–T37 and T29a to the action trigger. It also adds immutable reconciliation checks per reconciler lease generation, and two-person operator resolutions. The execution worker runs a fenced reconciler that applies the pinned proof standard. Only AUTHORITATIVE absence, after every call has settled and with no reported success, permits a retry with the same operation key or FAILED; BEST_EFFORT absence is STILL_UNKNOWN. Conflicts, exhaustion and contracts without proof go to NEEDS_HUMAN_RESOLUTION, and only once every call has settled. Operators resolve through `/v1/actions/{id}/resolutions` or `eacpctl action`. The resolver is separated from the subject and the agent's owners, and every retry needs a second operator. The flagship Fake ERP tests run the real HTTP connector, worker, sweeper and reconciler. Decisions are recorded in ADR-004 Rev 2.5. The review is in `docs/reviews/2026-09-23-phase7-code-review.md`.

Implement:

```text
UNKNOWN_OUTCOME from every cause in §19
reconciler lease
a proof standard per connector (§20.2)
"not found" under BEST_EFFORT → STILL_UNKNOWN (no retry)
NEEDS_HUMAN_RESOLUTION + operator resolve API/CLI (§20.3)
```

Flagship tests:

```text
Fake ERP executes → the response is lost → UNKNOWN_OUTCOME → the lookup finds it → SUCCEEDED
Fake ERP executes → delayed visibility → the lookup does not find it → no retry
    → NEEDS_HUMAN_RESOLUTION, or found later → SUCCEEDED
the worker is killed during EXECUTING → UNKNOWN_OUTCOME (no re-dispatch)
```

---

# 82. Phase 8 — Slice A Hardening & Demo

**Implementation status (2026-09-23): delivered; Slice A is complete.** Every §103 invariant tagged [A] maps to passing automated tests in `docs/INVARIANTS.md`, and `test/invariants` checks that map against this section and the test suite. The hardening work added:
- a catalog test of the RLS convention and of every reviewed cross-tenant path;
- a tenant sweep over every table after a full flow, and cross-tenant tests of the action API;
- chaos tests: repeated database connection loss under live loops, restarted services, a killed worker, and duplicate submissions;
- evidence reconstruction from an `action_id` over the API, with journal chain verification;
- the §111 demo (`scripts/demo.sh`, `docs/DEMO.md`), which runs against an isolated compose project and restarts the API, the worker and PostgreSQL.

Two findings changed behaviour. Cancelling a `RECEIVED` action (T5a, ADR-004 Rev 2.6, migration 00008) keeps a governance outage from blocking cancellation (invariant 18). Compose now restarts services that fail closed at startup. The review is in `docs/reviews/2026-09-23-phase8-code-review.md`.

Build:

```text
bypass tests (§3.2): a direct call to the Fake ERP fails; no secret leaks
tenant isolation tests against RLS
chaos: worker crash, DB restart, duplicate submission
the race detector over the whole test suite
evidence reconstruction: from an action_id you must get governance + approval + execution + outcome
Slice A demo script (§110)
```

Slice A's exit criteria = every invariant in §103 labelled [A] has a passing automated test

---

# 83. Phase 9 — AGT Sidecar PDP (Slice B)

> **Status (2026-09-24): complete.** Normative detail: ADR-002 Rev 2.4 §8, and `research/REFERENCES.md` for the upstream API verified by running it.

Implement:

```text
sidecars/agt-pdp: a Python service wrapping AGT/ACS (a pinned version)
    HTTP API: Evaluate → verdict + enforced payload + policy version + evidence
integrations/governance/microsoftagt: a Go client implementing GovernanceProvider
loopback / mTLS, timeout, fail closed
conformance tests: the local provider and the AGT provider must give the same verdict on a reference policy set
```

No change to the approval store or the action core (ADR-002)

---

# 84. Phase 10 — NATS JetStream (Slice B)

> **Status (2026-09-24): complete.** Normative detail: [ADR-014](adr/ADR-014-postgresql-authority-nats-signals.md) Rev 1.0. PostgreSQL stays the only authority; a work hint only wakes the worker's claim loop, and polling stays on.

Implement:

```text
outbox → NATS relay
inbox dedup
work-available hints (action_id only)
an event stream for dashboards
```

Correctness must not depend on NATS (§60).

---

# 85. Phase 11 — Budget Reservation (Slice B)

> **Status (2026-09-24): complete.** Normative detail: [ADR-012](adr/ADR-012-budget-reservation.md) Rev 1.0. The release transaction reserves on the agent's leaf only, and the action's own state change commits or releases the reservation without locking the account. Child limits are escrowed from their parent, and raising a limit is two-person.

Atomic:

```text
reserve
commit
release
```

Concurrency test:

```text
100 concurrent reservations
```

must never oversubscribe hard budget

including lock ordering, escrow, reservation TTL (§47), and measuring p99 under contention

---

# 86. Phase 12 — Fair Scheduler (Slice B)

> **Status (2026-09-24): complete.** Normative detail: [ADR-011](adr/ADR-011-scheduler-fairness.md) Rev 1.0. PostgreSQL orders claims by tenant/team weighted turns, pinned contract priority with aging, and connector capacity. The worker refreshes the hint after each claim; T14 remains authoritative. The 10,000:100:100 benchmark served the two small teams 10 claims each in the first 30, at 1.40 seconds per 30 claims on the development machine (`-race`, one worker).

Implement:

```text
tenant scheduling
team scheduling
priority
aging
connector capacity
```

The scheduler chooses the claim order in Postgres (§24)

Benchmark fairness

---

# 87. Phase 13 — Backpressure, Bulkheads, Circuit Breakers & Retry Budgets (Slice B)

> **Status (2026-09-24): complete.** Normative detail: [ADR-022](adr/ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) Rev 1.0. Admission adds a per-tenant limit on unreleased actions and a per-connector queue limit (429 with a scope). Each worker bulkheads its slots per capacity group. A per-worker breaker opens a journaled, shared connector circuit that PostgreSQL enforces at T14 and T16, and operators can disable a connector. Backoff is jittered, and the retry budget (attempts, elapsed time, retry cost) is enforced in PostgreSQL on every retry edge. Slice B is complete.

```text
global / tenant / connector / queue / worker limits
bulkhead per connector group
circuit breaker (per worker first + a shared "connector disabled" flag)
backoff + jitter
retry budget
```

---

# 88. Phase 14 — MCP Registry & Fingerprint (Slice C)

> **Status (2026-09-24): complete.** Normative detail: [ADR-023](adr/ADR-023-mcp-registry-and-tool-fingerprint.md) Rev 1.0. An MCP server is a connector with protocol `mcp`, and its tools are discovered, never declared. The execution worker's scanner lists them over Streamable HTTP (2026-07-28, with a fallback to the initialize-based revisions) under a fenced scan lease. PostgreSQL fingerprints and classifies every definition (`initial`, `low` for display-only changes, `high` otherwise) and keeps its history. A contract pins the definition its proposer reviewed; a high-risk change, or a certified tool that disappears, quarantines the tool and its contract stops matching. Quarantine is containment (operator or registry approver), and release needs a second registry approver and never recertifies. Executing MCP tools (`tools/call`) is out of scope.

```text
discovery
fingerprint
schema tracking
risk metadata
contract invalidation when the fingerprint changes
quarantine
```

---

# 89. Phase 15 — Dependency Graph & Blast Radius (Slice C)

> **Status (2026-09-24): delivered.** Normative detail: [ADR-015](adr/ADR-015-dependency-graph.md) Rev 1.0. The registry's active allowlist and connector/tool rows provide current capability edges; tenant-scoped evidence records Agent→Model, Agent→MCP, Tool→System and Agent→Agent with source, expiry and confidence. PostgreSQL recursive CTEs return confirmed and possible agent versions. Stale, lower-confidence and unknown edges widen the possible set. Coverage remains `observed_only`: missing declarations do not prove absence. Operators can query the radius by API or `eacpctl`; the result includes affected owning groups, contract data classes and production action count over the last 24 hours. No workflow entity exists yet, so no workflow count is claimed.

Store:

```text
Agent → Model

Agent → MCP

MCP → Tool

Tool → System

Agent → Agent
```

Implement blast radius queries

Rev 2: edges have a source, freshness and confidence.

If an edge is unknown or stale → the blast radius is taken to be **wider**, not narrower.

---

# 90. Phase 16 — Distributed Kill Switch (Slice C)

> **Status (2026-09-24): delivered for seven scopes.** Normative detail: [ADR-016](adr/ADR-016-distributed-kill-switch.md) Rev 1.0. PostgreSQL holds each kill scope's state and epoch plus a tenant epoch; `eacp.set_kill` requires an operator, a tenant-local target and one of AGT's four reason codes, and a second operator must resume. T14/T16 check the scopes under a tenant advisory lock, so a kill committed before the dispatch intent means no external call. The worker re-checks before the call and polls during it; a `kill.changed` outbox signal over NATS only wakes that check. An epoch change during execution records `UNKNOWN_OUTCOME` for reconciliation. Enforced scopes: tenant, team (the pinned owner group), agent, agent version, action, connector and tool. `global`, `run` and `model` are rejected until authoritative platform identity and action bindings exist. **Update:** `model` followed in Phase 25b (ADR-031) and `run` in Phase 28 (ADR-016 Rev 1.1), once PostgreSQL bound every Studio action to its run; `global` still waits for platform authority.

Integrate:

```text
AGT kill semantics
+
Control Plane distributed propagation
+
kill_epoch fenced check (§40)
```

---

# 91. Phase 17 — Fleet Operations (Slice C)

> **Status (2026-09-25): delivered.** Normative detail: [ADR-024](adr/ADR-024-fleet-operations.md) Rev 1.0. Pause, resume, quarantine, release and rollback are ADR-003 lifecycle transitions that PostgreSQL applies atomically: inserting a target row makes its transition under the version guard. So a fleet operation cannot grant what a single transition could not. Resume and release undo only what their source operation changed. A rollback activates an older `SUSPENDED` version of one agent. Every operation records its targets and their database-read `from` state, and it is journaled at commit. Selection is explicit (`all` is required for the whole tenant), a dry run returns the plan, and an operation changes at most 500 versions. The fleet list and health (`GET /v1/fleet/agents|health`, `eacpctl fleet status|list`) are read-only observations: active version, owner status, matching kills, capability drift, open circuits, and open and recent actions. They never decide. Canary and upgrade-with-evaluation stay with ADR-018; kill remains ADR-016.

Build:

```text
fleet list
health
pause
resume
quarantine
rollback
```

---

# 92. Phase 18 — Agent FinOps (Later)

> **Status (2026-09-25): delivered.** Normative detail: [ADR-025](adr/ADR-025-agent-finops.md) Rev 1.0.
> - **Ingest.** LLM cost is ingested, never proxied (§3.1). An agent exports OTLP/HTTP JSON GenAI spans with its own key; PostgreSQL binds each record to that key and prices it from an insert-only, forward-only rate card. Admins import provider billing lines.
> - **Effective spend.** Effective LLM spend per agent, unit and UTC day is the greater of the reported and billed sums.
> - **Chargeback** groups tool spend (ADR-012, committed and held) and LLM spend by agent, owning team or budget account. Accounts roll up the ADR-012 tree.
> - **Soft limits and alerts.** Monthly soft limits, soft-limit, anomaly and unpriced-usage alerts, and the dashboard are observations. Only ADR-012 hard limits block.
> - **Not done:** actual tool cost, run budgets, forecasts and an alert signal on NATS.

Build:

```text
cost telemetry (ingest; §45)
hierarchical budgets
chargeback
spend dashboard
anomaly alerts
```

---

# 93. Phase 19 — Release & Evaluation (Later)

> **Status (2026-09-25): delivered.** Normative detail: [ADR-018](adr/ADR-018-release-and-evaluation.md) Rev 1.0.
> - **AgentRelease** is a PostgreSQL row with a fixed plan: `EVALUATING → SHADOW → CANARY (steps) → PROMOTED`, or `ROLLED_BACK` from any open state. Every forward move is a second person's.
> - **Evaluation** results are recorded, attested evidence; EACP gates on them and does not run them.
> - **Replay** answers only from EACP's recorded outcome of a reference action, and gates on agreement rates (§51).
> - **Shadow** proposals go to an insert-only observation table; the candidate is not `ACTIVE`, so it has no path to execution (§52).
> - **Canary** activates the candidate beside the stable version for a deterministic subject cohort, enforced at T2, on every move toward execution and at T16. Guardrails compare denials, failures, unknown outcomes, latency and cost with the stable version.
> - **Rollback** is manual (operator or approver) or automatic on a sufficient report with a breach. The automatic path only withdraws; promotion stays human.
> - **Not done:** running evaluation harnesses, traffic mirroring, statistical tests and weight-based canaries.

Build:

```text
AgentRelease
Evaluation
Replay (research-grade)
Shadow (structurally non-destructive)
Canary
Rollback
```

---

# 93a. Phase 20 — Governance-as-Code (Later)

> **Status (2026-09-25): delivered.** Normative detail: [ADR-026](adr/ADR-026-governance-as-code.md) Rev 1.0. A bundle (`eacp.yml`, targets, variables) declares connectors, tools, contracts, agents, versions and allowlists. The API plans it in one snapshot into a change set whose digests PostgreSQL computes. Submit (one person) and approve (a second) run its steps through the same registry writes and triggers as the API; a stale change set runs nothing. Nothing is deleted, releases and containment are never bypassed, and drift is read-only.

---

# 93b. Phase 21 — Governance-as-Code: identity, policy and budgets (Later)

> **Status (2026-09-25): delivered.** Bundles declare principals, roles, groups and members, the tenant
> policy, budget accounts with hard and soft limits, and model prices (ADR-026 Rev 1.1). Every step
> writes through the same Tx types as the API, so the existing two-admin triggers decide it; prune never
> leaves fewer than two admins, and budget counters never make a plan stale.

Principals, groups, memberships and role grants; governance policy versions; budget accounts and limit changes (raising stays two-person); FinOps soft limits and forward-only prices. They use the Phase 20 engine.

---

# 94. Phase 22 — Agent SOC (Later)

> **Status (2026-09-25): 22a delivered (ADR-027).** Incidents are opened by a PostgreSQL evaluator from existing signals (MCP drift, kills, open circuits, unknown outcomes, canary rollbacks, FinOps overspend), with their blast radius. Operators work incidents through an insert-only, journaled timeline, and a critical incident is resolved by a second person. `GET /v1/soc/summary` serves the §55 counters from one read-only snapshot. **22b delivered (ADR-028):** the operator console at `/ui/` covers the areas below. It is embedded in controlplane-api, calls the existing API with the operator's own key (memory only) and holds no authority; containment (kill, tool quarantine, circuit disable, fleet pause) goes through a confirm dialog, and fleet operations are always dry-run first.
>
> **Phase 26-UI delivered (2026-09-29, ADR-028 Rev 1.1):** the console has a design system (semantic light/dark tokens contrast-tested to WCAG AA, KPI cards, banners, empty states, CSS-only icons) and speaks English and Thai, chosen with `?lang=` and never stored. It adds no route, table or migration.

> **Phase 26a delivered (2026-09-30, ADR-032):** the execution worker calls a certified MCP tool at most once per action after comparing the server's current definition with the certified one byte for byte; tools with `x-mcp-header` are refused; the reference is a digest of the result and the output is never stored (returning it is Phase 26b). Migration 00025 makes MCP contracts single-attempt.

> **Phase 26b delivered (2026-09-30, ADR-034):** the result channel. A contract may keep a success's output (HTTP `result`, the MCP `CallToolResult`, A2A artifacts) for the calling agent for 60 s to a day; PostgreSQL records it only from the lease holder's succeeded attempt, serves it only to an `ACTIVE` version of the action's agent and hides it from `eacp_app`; the sweeper clears it after expiry. Migration 00026.

> **Phase 27a-1 delivered (2026-09-30, ADR-033 Rev 1.1):** Agent Studio's data and rules. A `studio_author` saves a definition; PostgreSQL validates it, derives its capability and creates the agent, a `REGISTERED` version and an allowlist of exactly those tools; a registry approver other than the author approves (retiring the previous version) or rejects it once. `studio_runtime` is held alone by a service principal and proposes agent keys only for approved Studio versions. The author and approver API is `/v1/studio/...`. Migration 00027.
>
> **Phase 27a-2 delivered (2026-09-30, ADR-033 Rev 1.2):** `agent-runtime` runs approved Studio agents through the API only. A department member starts a run; the runtime leases it, derives the version's key from a master secret only it holds (HMAC-SHA-256, `identity.KeyFromSecret`), sends each `tool_call` as an ordinary action with the requester as subject and the idempotency key `studio:<run>:<index>`, re-reads earlier outputs through the result channel and finishes with an answer kept one hour for the requester. Runs fail closed with named reasons; the runtime proposes keys and successors (approved by a `registry_approver`), an operator revokes every Studio key at once, and an expiring key opens an incident. Migration 00028; the image, compose and Helm are Phase 27a-3.

> **Phase 27a-3a delivered (2026-09-30, ADR-033):** `agent-runtime` ships in the image, the release binaries, compose (profile `studio`, the `agents` network only) and the Helm chart (`studio.enabled`, egress to the API only). Fake MCP gains `get_leave_balance` on a second instance, `fakemcp-hr`, and `DEMO=S` runs the Agent Studio demo from a template through approval to a run, a tool error, a revocation and a secret scan. The `/studio/` page is Phase 27a-3b.

> **Phase 27a-3b delivered (2026-09-30, ADR-028 Rev 1.2):** `/studio/`, a second page built from the console's modules and rules: the leave-balance template, a form for new agents and new versions, each agent's stage in plain words with who acts next, the approver's queue for requests and runtime keys, and runs with their answer or reason, in English and Thai. `/v1/me` lists the caller's groups. Phase 27a (the thin slice) is complete; the department gate comes next.

> **The gate and Phase 27b (2026-09-30, ADR-033 Rev 1.3):** no department tried the thin slice; the owner chose the fallback, so the gate is recorded as not met and 27b was built as the program spec describes. The Agent Hub: department leads, listings proposed by the owner and published by a lead (department) or an admin or registry approver (organisation), visibility, runs and copies decided by PostgreSQL; without a listing only the owner runs an agent.

> **Phase 27c delivered (2026-10-01, ADR-033 Rev 1.4):** schema-v2 forward tool/model/branch/answer graphs, exact PostgreSQL capabilities, fenced one-use model intents and private typed output, precise branches, safe recovery, connected node editing, local sample tests and confirmed approved-version preview are implemented. Compose proof, optional gateway Helm rendering, the full race suite, examples, compose security and UI checks passed. The complete live Kubernetes end-to-end gate passed with two nodes and enforced Calico policies, including full Studio, runtime isolation and disruption. The department gate stays not met; do not start Phase 29 automatically.

Operator UI:

```text
Inventory

Fleet

Security

Approvals

Execution

Dependencies

Cost

Incidents
```

---

# 95. Phase 23 — Kubernetes / HA (Later)

> **Status (2026-09-26): delivered (ADR-029 Rev 1.1).** **23a:** No leader: PostgreSQL arbitrates every background loop. The incident, FinOps and release evaluators take a per-tenant transaction-scoped try-lock so N replicas evaluate each tenant once per interval; the sweeper relies on row locks and tolerates actions another replica moved first. On SIGTERM a service fails `/readyz`, stops its loops and keeps serving for `EACP_SHUTDOWN_DELAY`. A multi-replica test (three API loop sets, three workers, one of each stopped mid-run) proves the whole. **23b:** the Helm chart `deployments/helm/eacp` (docs/KUBERNETES.md) runs the API, the worker and the PDP as hardened, replicated Deployments with the compose boundary as NetworkPolicies, secrets referenced by name only, a migration hook, PodDisruptionBudgets, surge-only rolling updates, grace periods that cover a drain and an in-flight call, and optional CPU HPAs (queue-depth scaling deferred). `test/helm` checks every rule in the rendered YAML; `scripts/k8s-e2e.sh` runs the Slice A demo and a disruption run (API rollout, workers 2→3, a node drained) on a 2-node minikube cluster with Calico under the `restricted` Pod Security Standard.

After architecture stable:

```text
Helm

multiple API nodes

multiple scheduler nodes

worker autoscaling

Pod disruption

leader election where required
```

---

# 96. Phase 24 — JIT Credentials (Later)

> **Status (2026-09-26): 24a delivered (ADR-019 Rev 1.0).** The execution worker resolves every connector credential through a provider. Besides the static secret, a secrets-file entry may name an OAuth 2.0 client-credentials provider: the worker mints a short-lived Bearer token (at most one hour) just in time, reuses it only while it outlives the whole call, and never persists it. A failing token endpoint dispatches nothing and withholds the binding from claims during a 1–60 s back-off. Minted tokens are redacted from every log until a day after they expire. Fake ERP gains a token endpoint; `TestJITDemo` buys with minted tokens on compose and Kubernetes and finds no client secret or token in responses, logs or the database. **24b delivered (ADR-019 Rev 1.1):** workload identity federation. An `oauth2` entry may authenticate with `client_assertion_file` instead of a client secret: the worker presents a platform-issued JWT (a Kubernetes projected service-account token, as Azure Workload Identity uses) per RFC 7523, re-read at every mint, and holds no long-lived secret for the binding. Tokens may live up to 24 h (Entra ID issues 60–90 minutes) but are used for at most an hour. The chart gives the worker its own ServiceAccount and an optional projected token; `TestFederatedJITDemo` buys on minikube with tokens minted against the cluster's real issuer. **24c delivered (ADR-019 Rev 1.2):** `private_key_jwt`. Where no platform issues the worker an identity, an `oauth2` entry may hold the worker's own RSA or P-256 key (and optionally its certificate): every mint signs a fresh five-minute assertion with a unique `jti` (RS256, PS256 with `x5t#S256` as Entra ID expects, or ES256), so the IdP stores only a public key. The key is read once, redacted and never sent to a connector. Fake ERP gains a key client that verifies against a JWKS and accepts each `jti` once, across restarts; `eacpctl dev-client-key` writes a development key, certificate and JWKS. `TestPrivateKeyJWTDemo` buys on compose (the key mounted into the worker only) and on minikube (the key inline in the worker's secrets). **24d delivered (ADR-019 Rev 1.3, 2026-09-27):** HashiCorp Vault KV v2 as a credential source. A static credential, an OAuth client secret and a `private_key_jwt` key and certificate may each name a Vault KV v2 path and key (`value_vault`, `client_secret_vault`, `key_vault`, `certificate_vault`) instead of holding the value. The worker logs in with Kubernetes auth (its own projected token, audience `vault`, from the chart's `worker.vaultIdentity`) or AppRole, never at load, and reads each path when it needs it, cached for a refresh interval (30–3600 s); a stale value is never served, a rotation takes effect within one interval (at once after the target rejects the old value), and a Vault failure withholds only that binding's work with the usual back-off. Vault tokens and values are redacted and never persisted. `TestVaultDemo` buys through both kinds of binding on compose (AppRole) and on minikube (Kubernetes auth), withholds a purchase while the value is deleted in Vault and completes it once restored. **24e delivered (ADR-019 Rev 1.4):** SPIFFE JWT-SVIDs. A top-level `spiffe` block names the SPIRE agent's Workload API socket and the worker's SPIFFE ID; a static credential may be the worker's JWT-SVID for the connector's audience (`value_spiffe`, sent as the Bearer), and an `oauth2` entry may authenticate with a JWT-SVID as its client assertion (`client_assertion_spiffe`). The worker never contacts the agent at load, fetches per audience on demand, refuses an SVID for any other identity, never sends one that could expire during the call, and withholds only that binding's work with the usual back-off while the agent is down or refuses. SVIDs are redacted until a day after they expire. The chart's `worker.spiffe` mounts the Workload API into the worker pod alone through the SPIFFE CSI driver; Fake ERP verifies JWT-SVIDs against a SPIFFE bundle as an OAuth client and as a Bearer. `TestSPIFFEDemo` buys through both kinds of binding on minikube against a real SPIRE 1.15.3. **24f delivered (ADR-019 Rev 1.5):** OAuth 2.0 token exchange (RFC 8693). An `oauth2` entry with `"grant": "token_exchange"` trades a subject token (the worker's projected token, re-read from its file at every mint, or its JWT-SVID) for an access token at an STS, with client authentication optional; the response must name an access token (`issued_token_type`). With `impersonate` the exchanged (federated) token only buys a GCP service account's token from a `generateAccessToken` endpoint, and that token goes to the connector. Each token goes to one place only; every 24a rule applies to the final token, and every failed step withholds only that binding with the usual back-off. Subject and federated tokens are redacted until a day after they expire. Fake ERP gains an STS (principal `sts:<sub>`, subject digests audited) and impersonation (principal `sa:<email>`); `TestTokenExchangeDemo` buys through a projected-token exchange and a JWT-SVID exchange with impersonation on minikube. **24g delivered (ADR-019 Rev 1.6): Phase 24 complete.** AWS STS web identity and SigV4. An `aws` entry assumes a role with `AssumeRoleWithWebIdentity` (unsigned, regional endpoint by default) using the worker's projected token or JWT-SVID, and receives temporary keys used for at most an hour; the HTTP connector signs execute and lookup with SigV4 (the pinned `aws-sdk-go-v2` signer, at the real clock) for the binding's region and service. The secret key never leaves the worker, the session token rides only on signed calls to the bound host, both are redacted until a day after the keys expire, and MCP servers are never signed (`unsupported_credential`). Fake ERP gains an AWS STS and SigV4 verification (principal `aws:<assumed-role ARN>`, key ids audited); `TestAWSDemo` buys through both subject sources on minikube. The five 24f follow-ups are fixed. **Later:** Vault dynamic secrets and HSM/KMS-held keys behind the same seam.

Implement:

```text
CredentialProvider
```

then:

```text
SPIFFE
Cloud identities
Vault
```

Credential custody (agents hold no credentials) has been enforced since Slice A.

This phase only moves from static secrets to short-lived credentials.

---

# 97. Phase 25 — A2A & LLM Gateway (Later, optional modules)

A2A: support governed remote Agent execution

```text
Agent A
 ↓
A2A
 ↓
Agent B
```

authority stays governed

execution remains observable

**Delivered (Phase 25a, 2026-09-27, ADR-030):** outbound A2A 1.0 delegation as a connector. A remote agent is a connector with protocol `a2a` whose one tool, `delegate`, is discovered from its Agent Card and certified over the whole card; a delegation is an ordinary action sent at most once by the execution worker, followed with `GetTask`, cancelled when EACP stops following it, and settled by a human when its outcome is unknown. The LLM Gateway followed in Phase 25b; inbound A2A followed in Phase 29.

LLM Gateway (optional): a new ingress on the same shared core (§3.1)

```text
identity + capability + GovernanceProvider + audit + budget
```

**Delivered (Phase 25b, 2026-09-27, ADR-031):** the `llm-gateway` service. Agents send Anthropic Messages or OpenAI Chat Completions requests, streamed or not, with their own EACP key; the gateway authenticates the agent, asks the PDP (metadata only, never content), admits the call in PostgreSQL against a per-version model allowlist, kill scopes (including the new `model` scope) and a hard budget reservation at the rate card's price, forwards it with a provider key only the gateway holds, cuts it within seconds when killed, and settles its priced usage. An insert-only ledger (`eacp.llm_calls`) records every call without content; a sweeper settles abandoned calls. Inbound A2A followed in Phase 29.

**Delivered (Phase 29, 2026-10-02, ADR-030 Rev 1.1):** opt-in A2A 1.0 JSON-RPC on controlplane-api accepts one structured governed action using an existing approved EACP agent key. `SendMessage`, `GetTask` and `CancelTask` project the shared action engine; tasks use action UUIDs and existing idempotency, ownership, governance, approval, budgets and kill rules. Unknown outcomes stay working; cancellation is final only when PostgreSQL records it. Artifacts use the private retained result channel. There is no new schema, task store or service. Full race, compose/SDK replay, all 11 live Kubernetes/Calico scenarios, examples and security passed ([evidence](superpowers/specs/spec-phase-29-inbound-a2a/VERIFICATION.md)).

---

# 98. Flagship Demo — Procurement

**Rev 2 — there are two levels; the first must use Slice A only**

Slice A demo (Phase 8):

```text
Employee
 ↓
Procurement Agent            (no ERP credential and no network path to the ERP)
 ↓
EACP Action API              (agent identity + capability allowlist)
 ↓
local GovernanceProvider     (policy bundle v1: PO > 1M THB → escalate)
 ↓
PENDING_APPROVAL             (Manager + Finance, SoD enforced, durable)
 ↓
Release boundary             (revalidate → consume grant → QUEUED)
 ↓
Worker                       (lease + fenced dispatch intent + operation key)
 ↓
Fake ERP                     (the credential is held by the worker only)
 ↓
Create PO 2.4M THB
```

Full demo (after Slice B/C):

```text
... → AGT / ACS sidecar → ... → Budget reserve → Fair scheduler → Worker → SAP MCP → ...
```

---

# 99. Failure Scenario

SAP (Slice A: Fake ERP):

```text
PO successfully created
```

but:

```text
network timeout
```

Control Plane:

```text
UNKNOWN_OUTCOME
 ↓
Reconcile
 ↓
ERP lookup by operation key (AUTHORITATIVE)
 ↓
PO-9822 exists (external_reference = operation key)
 ↓
CONFIRMED_SUCCESS
 ↓
SUCCEEDED + outcome evidence
```

**Rev 2 — variants that must be demonstrated too:**

```text
Delayed visibility: the PO exists but a lookup does not find it yet
 ↓
"not found" but BEST_EFFORT → STILL_UNKNOWN → no retry
 ↓
a later lookup finds it → SUCCEEDED
or the attempts run out → NEEDS_HUMAN_RESOLUTION → an operator resolves it with evidence
```

```text
Kill worker mid-dispatch
 ↓
the lease expires during EXECUTING
 ↓
UNKNOWN_OUTCOME (no re-dispatch)
 ↓
reconcile → SUCCEEDED
 ↓
The Fake ERP has exactly one PO
```

---

# 100. Security Incident Demo (Slice C)

MCP fingerprint changes

```text
sap-mcp

fingerprint:
ABC → XYZ
```

Control Plane:

```text
Detect drift
 ↓
Connector contract invalidated
 ↓
Dependency Graph
 ↓
41 Agents affected
 ↓
Distributed kill / quarantine
 ↓
Block new execution
 ↓
Incident created
```

---

# 101. Release Demo (Later)

Agent:

```text
v14 production
v15 candidate
```

Pipeline:

```text
Replay
 ↓
Shadow
 ↓
Canary 5%
 ↓
Monitor SLO + cost + policy
 ↓
Promote
```

Or:

```text
Rollback
```

---

# 102. Testing Strategy

Unit:

```text
domain state machines (property-based: no path that breaks ADR-004)
scheduler
budget
retry
digest (JCS test vectors)
dependency graph
approval eligibility / SoD
```

Integration:

```text
PostgreSQL (including RLS)
NATS (Slice B)
AGT adapter (Slice B, conformance vs local provider)
Fake ERP
```

Concurrency:

```text
lease race
approval consumption
idempotency
budget race
stale worker dispatch
reclaim during EXECUTING
```

Security:

```text
tenant isolation
approval replay
parameter substitution
transform-then-approve
self-approval / cross-tenant approval
stale approval after policy change
control-plane bypass (direct ERP call)
MCP drift
credential leak (API, payload, journal, trace, log)
```

Chaos:

```text
worker crash
worker crash mid-dispatch
DB outage
NATS disconnect (Slice B)
PDP outage (fail closed; containment still works)
connector outage
duplicate message
duplicate submission
timeout after external commit
delayed visibility at external system
```

---

# 103. Critical Invariants

The labels [A]/[B]/[C] name the slice by which the invariant must have a passing automated test.

Every original (Rev 1) invariant is kept, with clearer explanations:

```text
1. [A] A stale worker cannot commit.
       Rev 2: and cannot cause a second dispatch of a non-idempotent action (invariant 12).

2. [A] One-time approval cannot release two execution claims.

3. [B] Hard budgets cannot oversubscribe.

4. [A] Duplicate queue messages must not imply duplicate external effects.
       Rev 2: nor may duplicate submissions or worker reclaims.

5. [A] Timeout after dispatch does not automatically mean failure.

6. [A] Irreversible non-idempotent action is never blindly retried.

7. [A] Sensitive action governance is revalidated before execution.

8. [A] Tenant isolation cannot be bypassed.

9. [B] Connector failure cannot starve unrelated connector pools.

10. [A] Audit/evidence references remain reconstructible end-to-end.
```

Added in Rev 2:

```text
11. [A] Agents never hold credentials for privileged external systems;
        privileged side effects are performed only by EACP-controlled workers or execution proxies.

12. [A] Once dispatch intent is recorded, an action is never re-dispatched unless
        (a) the connector is READ_ONLY or natively idempotent,
        (b) reconciliation produced connector-certified authoritative negative evidence, or
        (c) an authenticated human resolution authorised it —
        and every re-dispatch reuses the same operation key (ADR-004).

13. [A] "Not found" alone never authorizes retry of an irreversible or non-idempotent action;
        retry requires connector-certified authoritative negative evidence
        or an authenticated human resolution.

14. [A] An action executes only its enforced payload, whose digest matches
        the governance decision and approval grant it was released under.

15. [A] An approval grant is bound to tenant + action + enforced_digest + policy_version,
        expires, is consumed at most once,
        and cannot be granted by the requesting subject or the agent's owner.

16. [A] Approval and execution state are durable: a restart of any EACP process
        loses no pending approval, grant, or in-flight action state.

17. [A] Every state transition and privileged operator action is recorded
        in an append-only, hash-chained journal with actor and reason.

18. [A] Governance failure fails closed for new side-effecting actions,
        but never blocks cancellation, reconciliation reads, or containment.

19. [A] Every executed action is attributable to an authenticated agent identity
        and an ACTIVE AgentVersion whose capability allowlist includes the tool.
```

---

# 104. Load Benchmark

Workloads:

```text
100 agents

1,000 agents

5,000 agents

10,000 agents
```

Measure:

```text
Admission throughput

Scheduling throughput

Queue wait

Lease latency

Budget reservation

Idempotency latency

Execution overhead

CPU

RAM

DB utilization

NATS utilization
```

percentiles:

```text
P50
P95
P99
```

Status: measured by `scripts/bench.sh` (`cmd/eacp-bench`, `internal/bench`; design in
`docs/superpowers/specs/2026-09-28-load-benchmark-design.md`) on an isolated compose stack, open-loop, under the
local and the AGT PDP and through the LLM gateway. The latest run is committed in `docs/benchmarks/` and rendered as
`docs/BENCHMARKS.md`; §105 is enforced by `TestBenchmarksDocMatchesBaseline`, `TestReadmeNumbersComeFromTheBaseline` and
`TestFindingsQuoteTheTables`.
Not yet measured: several tenants, several workers or replicas, Kubernetes.

---

# 105. Never Fake Benchmarks

The README must never say:

```text
100k actions/sec
```

until it is measured.

Separate:

```text
Control Plane overhead

Governance latency

External API latency

LLM latency
```

---

# 106. Development Rules for Codex / Claude Code

Every phase:

```text
READ MASTER PLAN

 ↓

READ relevant ADR

 ↓

INSPECT existing code

 ↓

STUDY relevant upstream reference

 ↓

STATE invariants

 ↓

WRITE tests

 ↓

IMPLEMENT

 ↓

RUN tests

 ↓

RUN race detector

 ↓

UPDATE docs
```

---

# 107. AI Development Rule

Codex/Claude must never:

```text
invent API without verifying upstream

copy external project without attribution

disable test to make CI green

silently weaken fail-closed behavior

claim exactly-once without proof

implement next phase automatically

resolve an ambiguous safety/correctness assumption optimistically   (Rev 2: always choose the conservative option first)
```

---

# 108. Upstream Contribution Strategy

The Execution Fabric / Control Plane is an independent project first,

then integrates with AGT.

The path:

```text
Standalone Control Plane

 ↓

Working AGT integration

 ↓

Example

 ↓

Discussion with Microsoft maintainers

 ↓

Small upstream contribution

 ↓

Reusable integration

 ↓

Possible provider / protocol proposal
```

---

# 109. Potential Microsoft Contributions

If the maintainers are interested:

```text
PostgreSQL ApprovalStore (Rev 2: more important, because ACS is stateless and the Go SDK has no approval chain yet — issue #3083)

Distributed execution integration example

Outcome attestation bridge

Execution provider abstraction

Conformance tests

Go parity fixes
```

Do not propose a massive PR before a discussion.

---

# 110. MVP Definition

The MVP is not the whole system.

**Rev 2 — the MVP is Slice A, followed by Slices B and C:**

## Slice A — Correct, non-bypassable execution (Phase 1–8)

The goal statement that automated tests and the demo must prove:

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

```text
Agent Registry (minimal) + agent identity + capability allowlist
Credential custody: secrets at worker only; network-isolated Fake ERP
GovernanceProvider interface + local provider (versioned policy bundles)
EACP-owned approval store (SoD, quorum, expiry, policy-version binding)
Action API (idempotency, ?wait=) + ADR-004 state machine
Atomic release boundary
PostgreSQL (RLS) as sole execution authority; outbox table
Worker: lease, heartbeat, generation fencing, fenced dispatch intent
Connector framework + Fake ERP (operation key, lookup, failure modes)
UNKNOWN_OUTCOME + reconciliation with proof standard
NEEDS_HUMAN_RESOLUTION + operator resolution
Hash-chained audit / evidence journal
OpenTelemetry (basic)
```

Not in Slice A: the AGT sidecar, NATS, budgets, the fair scheduler, the dependency graph, the distributed kill switch, FinOps

## Slice B — Enterprise governance & capacity (Phase 9–13)

```text
AGT / ACS sidecar PDP
NATS JetStream (hints / events)
Hard budget reservation
Fair scheduler
Backpressure, bulkheads, circuit breakers, retry budgets
```

## Slice C — Fleet safety (Phase 14–17)

```text
MCP registry + fingerprint + contract invalidation
Dependency graph + blast radius
Distributed kill switch
Fleet operations
```

Each slice must pass the invariants labelled with that slice in §103 before the next slice can start.

---

# 111. Portfolio-Ready Definition

**Rev 2 — phases are grouped by slice, and each slice's demo uses only capabilities that really exist**

Slice A demo:

```text
Register Agent (owner + tool allowlist)

Agent tries to call Fake ERP directly → fails (no credential, no route)

Agent requests tool outside allowlist → DENIED

Govern action (local provider)

Approve high-risk action (SoD enforced; self-approval rejected)

Restart API + worker while approval pending → approval still there

Execute through Go Fabric

Kill worker mid-dispatch → UNKNOWN_OUTCOME → reconcile → exactly one PO

Duplicate submission / reclaim → no duplicate external effect

Execute then timeout → reconcile outcome

Delayed visibility → no retry → human resolution with evidence

Show audit / evidence chain for one action_id (governance → approval → execution → outcome)
```

Slice B demo:

```text
Same flow through AGT sidecar
Budget race: 100 concurrent reservations, no oversubscription
Fairness under a flooding tenant
```

Slice C demo:

```text
Trigger MCP drift
Show blast radius
Kill affected Agent version
Show trace + audit evidence
```

Status: the Slice C demo runs as `TestSliceCDemo` (`scripts/demo.sh`, `docs/DEMO.md`) with a fake MCP server (`cmd/fakemcp`).

---

# 112. Open-Source Ready Definition

```text
README

Architecture

ADRs

Threat model

Docker Compose

Examples

CI

Benchmarks

Contributor guide

License

Third-party attribution

Issue templates

Release process
```

> **Status (2026-09-28): delivered.** Every item above is in the repository and `test/opensource` keeps it there: the README and its Thai translation (with console screenshots taken from a running stack by `scripts/screenshots.sh`), [ARCHITECTURE.md](ARCHITECTURE.md), the ADRs, the [threat model](security/THREAT_MODEL.md), Docker Compose, three runnable [examples](../examples/README.md) run nightly, tiered CI in GitHub Actions, the [benchmarks](BENCHMARKS.md), the contributor guide, the Apache-2.0 license with NOTICE and THIRD_PARTY_NOTICES.md, issue and pull request templates, and the release process ([RELEASING.md](RELEASING.md), a tag-triggered workflow and CHANGELOG.md). User and contributor documents come in English and Thai (`X.md` and `X.th.md`) with matching outlines. The repository is ready to push; creating the GitHub repository is the owner's step.

---

# 113. Enterprise-Ready Direction

Later:

```text
Kubernetes HA

SPIFFE

SSO

RBAC

SIEM integration

Multi-region

Data residency

Managed secrets

Disaster recovery

Enterprise database HA

Long-term audit retention

Compliance exports

LLM Gateway module (optional, §3.1)
```

---

# 114. Main Differentiator

Do not compete on:

```text
we have more policies than AGT

we have a better agent framework than LangGraph

we have a better workflow engine than Temporal
```

What our Control Plane should stand out for:

> **Unified enterprise operations for governed autonomous agents.**

Including:

```text
Who owns the agent?

What can it do?

What does it depend on?

Should this action happen?

How should it execute?

Did it actually happen?

How much did it cost?

What else will break if we disable it?

How do we stop it immediately?
```

---

# 115. Product Story

Microsoft AGT:

```text
Governance primitives
```

Execution Fabric:

```text
Distributed execution correctness
```

Control Plane:

```text
Enterprise fleet management
+
security
+
operations
+
FinOps
+
release management
+
dependency intelligence
```

Together they form:

```text
Enterprise Autonomous Agent Infrastructure
```

---

# 116. Final Architecture

```text
┌───────────────────────────────────────────────────────────────┐
│                    AGENT APPLICATIONS                         │
│                                                               │
│ OpenAI │ Claude │ LangGraph │ MAF │ AutoGen │ Custom         │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                              ▼
╔═══════════════════════════════════════════════════════════════╗
║                ENTERPRISE AGENT CONTROL PLANE                ║
║                                                               ║
║  Registry                Dependency Graph                     ║
║  Fleet Management        Security Operations                 ║
║  FinOps                  Release Management                  ║
║  Evaluations             Incident Management                 ║
║  Operator UI             API / CLI                           ║
║  Agent Identity · Capability Allowlist · Approval Store      ║
║  Hash-chained Audit Journal                                  ║
╚═════════════════════════════╤═════════════════════════════════╝
                              │
                              ▼
┌───────────────────────────────────────────────────────────────┐
│                  GOVERNANCE FOUNDATION                        │
│                                                               │
│   GovernanceProvider ── Microsoft AGT / ACS (sidecar PDP)     │
│                                                               │
│ Policy │ Identity │ Trust │ Verdicts │ Context │ Audit        │
└─────────────────────────────┬─────────────────────────────────┘
                              │
                       Governance Decision
                              │
                              ▼
╔═══════════════════════════════════════════════════════════════╗
║             DISTRIBUTED EXECUTION FABRIC — GO                ║
║                                                               ║
║ Atomic Execution Boundary                                    ║
║ Fenced Dispatch Intent + Operation Key                       ║
║ Credential Custody (agents hold no enterprise credentials)   ║
║ Fair Scheduler                                               ║
║ Durable Queue                                                ║
║ Lease + Fencing                                              ║
║ Budget Reservation                                           ║
║ Idempotency                                                  ║
║ Retry Safety                                                 ║
║ UNKNOWN_OUTCOME                                              ║
║ Reconciliation (proof standard) + Human Resolution           ║
║ Backpressure / Bulkhead / Circuit Breaker                    ║
╚═════════════════════════════╤═════════════════════════════════╝
                              │
                              ▼
                   MCP / A2A / HTTP / gRPC
                              │
             ┌────────────────┼────────────────┐
             ▼                ▼                ▼
            SAP            Databases         GitHub
```

---

# 117. One-Sentence Positioning

> **Enterprise Agent Control Plane is an open infrastructure platform for governing, operating, securing, scheduling, and reliably executing autonomous AI agents across enterprise systems.**

---

# 118. Technical Positioning

> **Microsoft AGT governs agent actions; the Go execution fabric executes them safely; the Enterprise Agent Control Plane manages the entire agent fleet.**

---

# 119. Final Goal

Early stage:

```text
Strong Go portfolio project
```

Middle stage:

```text
Real open-source project
```

Long term:

```text
Enterprise Agent Infrastructure Platform
```

The heart of the system is not building one more agent,

but:

> **Building infrastructure that lets an organisation put large numbers of agents into production while still controlling their risk, behaviour, cost and impact.**

That is Enterprise Agent Control Plane.
