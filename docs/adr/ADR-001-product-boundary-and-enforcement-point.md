# ADR-001: Product Boundary and Enforcement Point

- **Status:** Accepted
- **Date:** 2026-09-23
- **Phase 0 gate:** yes
- **Related:** MASTER_PLAN §1, §3.1, §3.2, §45, §65, §70, §103 (inv. 11, 19); ADR-002, ADR-005, ADR-019, ADR-021
- **Review input:** `docs/reviews/2026-09-23-master-plan-review.md` (C5, H3, H4)

## Context

EACP's core promise (§1) is: *"Teams may build agents however they want, but enterprise resources are accessed through the Control Plane."* Revision 1 of the plan left two things open:

1. **Which call paths go through EACP.** FinOps (§45) assumed EACP could see LLM spend, but nothing said whether EACP proxies LLM calls.
2. **What makes the promise true.** The only mechanism that stops an agent calling SAP directly is keeping credentials away from the agent. That was scheduled for Phase 22 (JIT credentials), so every earlier phase would govern only the agents that chose to cooperate.

If an agent holds the SAP service account, EACP's registry, governance, approvals, audit and blast-radius answers are all silently incomplete. A control plane you can bypass is an observability tool, not a control plane.

## Decision

### 1. Scope: Agent Tool / Action Control Plane

For the MVP (Slices A–C), EACP sits in the path of **tool/action calls that touch enterprise resources**: ERP, databases, source control, messaging, MCP tools and internal APIs.

EACP is **not an LLM gateway** in the MVP:

- LLM calls go straight from the agent runtime to the provider.
- LLM usage and cost are **ingested** from OTel GenAI spans and provider billing exports, not proxied (§45).

### 2. Designed so an LLM gateway can be added later

The core is split into **ingress adapters** and a **shared core**:

| Shared core (ingress-agnostic) | Ingress adapters |
|---|---|
| Principal authentication | Tool/Action API (Slice A) |
| Capability check | LLM Gateway (later, optional) |
| `GovernanceProvider` (ADR-002) | A2A ingress (later) |
| Approval store (ADR-005) | |
| Audit/evidence journal | |
| Budget (Slice B) | |
| Telemetry | |

Shared-core types must stay generic (`operation`, `target`, `payload`, `side_effect_class`), with no "tool call" or "chat completion" specific shapes. A future LLM gateway is a new ingress that reuses the core without changing it.

### 3. Enforcement point: credential custody, capability and network (Slice A)

This is the governing principle, and it's an invariant (§103, #11):

> **Agents never hold credentials for privileged external systems. Privileged side effects are performed only by EACP-controlled workers or execution proxies.**

Slice A enforces it with four mechanisms:

| Mechanism | Slice A implementation |
|---|---|
| **Credential custody** | Connector secrets are loaded only by execution workers (env or mounted file). The DB holds only `ConnectorSecretRef`, never values. No API returns secrets. Secrets never appear in payloads, journal, traces or logs, and redaction is tested. |
| **Agent identity** | Each agent authenticates to EACP with its own credential. In Slice A that's a per-agent API key, hashed at rest; SPIFFE comes later. That credential is useless against enterprise systems. |
| **Capability allowlist** | An agent may request only tools in the allowlist of its **ACTIVE** `AgentVersion`. This is checked deterministically **before** governance and fails closed (`DENIED`, reason `capability`). |
| **Network segmentation** | In the demo, the agent's docker network has no route to Fake ERP. In production, the documented deployment requirement is to block egress from agent runtimes to privileged systems. |

An **execution proxy** is any EACP-controlled component that holds credentials and performs side effects under EACP's lease, fencing and journal rules. In Slice A that's the execution worker only. A proxy deployed next to a target system that accepts only EACP-authenticated requests is a future option. It must follow the same ADR-004 execution semantics.

### 4. Agent integration contract

- Agents call `POST /v1/actions` with an `Idempotency-Key`, and may pass `?wait=<duration>` to receive a synchronous result.
- A non-terminal state (`PENDING_APPROVAL`, `QUEUED`, `UNKNOWN_OUTCOME`, …) returns `202` with `action_id`. The agent follows up with `GET` or SSE.
- Agent-side adapters/SDKs **must not retry on their own** after a `202` or a transport error. They resubmit with the **same** `Idempotency-Key`, which EACP deduplicates.

### 5. Out of scope (explicitly)

- Governing actions that need no privileged credential, such as pure text generation.
- Preventing an organisation from issuing privileged credentials to agents outside EACP. That's an organisational control. EACP documents the requirement, and bypass *detection* (for example, external-system audit logs showing non-EACP principals) is a later capability.
- LLM prompt/response content governance (the LLM gateway module, later).

## Consequences

**Positive**
- The §1 promise becomes testable in Slice A: a direct call fails, an out-of-allowlist call is denied, and no secret leaks.
- A narrow product surface. It avoids competing with LLM gateways (§114).
- The ingress/core split keeps an LLM gateway cheap to add later.

**Negative / costs**
- Agent frameworks need a thin EACP tool adapter; teams can't just drop in their own API clients.
- The synchronous path adds latency on every privileged tool call. There's no SLO until it's measured (§105).
- LLM cost data depends on agents emitting OTel GenAI spans or on billing exports. It's eventually consistent and can't be enforced in real time without the gateway module.
- Network segmentation is a deployment responsibility EACP can document and demo but can't enforce everywhere.

## Conservative defaults for unresolved points

| Open point | Conservative default chosen |
|---|---|
| Tool has no connector contract | Not executable (fail closed). |
| AgentVersion not ACTIVE | Submission rejected. Also rechecked at release and at dispatch intent. |
| Unknown side-effect class | Treated as `IRREVERSIBLE_WRITE`, non-idempotent (at-most-once). |
| Adapter unsure whether a submit reached EACP | Resubmit with the same `Idempotency-Key`, never a new one. |

## Verification (Slice A exit)

- `test/security`: an agent container calling Fake ERP directly fails (no credential **and** no route).
- `test/security`: a tool outside the allowlist → `DENIED(capability)`, with an audit event.
- `test/security`: a secret canary value never appears in API responses, action rows, journal, traces or logs.
- `test/security`: submission with a SUSPENDED AgentVersion is rejected, and suspension between release and dispatch prevents dispatch.

## Alternatives considered

- **LLM gateway in the MVP.** Rejected: it's large, latency-critical, crowded with existing products, and not the differentiator.
- **Defer credential custody to Phase 22 (Rev 1).** Rejected: the core principle would be unenforced for most of the roadmap.
- **Agent-side SDK enforcement only.** Rejected: the agent is untrusted (§67), so enforcement has to sit where the credentials live.
