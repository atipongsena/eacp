# Architecture Decision Records

These ADRs hold the normative detail behind `docs/MASTER_PLAN.md` (Revision 2). **When an ADR and the master plan disagree, the ADR wins.** Fix the plan to match.

| ADR | Title | Status | Phase 0 gate |
|---|---|---|---|
| [ADR-001](ADR-001-product-boundary-and-enforcement-point.md) | Product Boundary and Enforcement Point | Accepted (Rev 2.1) | ✔ |
| [ADR-002](ADR-002-agt-integration-sidecar-pdp.md) | AGT/ACS Integration via Sidecar PDP | Accepted (Rev 2.4) | ✔ |
| [ADR-003](ADR-003-agent-registry-identity-and-capability.md) | Agent Registry, Identity and Capability Model | Accepted (Rev 1.3) | Phase 2 |
| [ADR-004](ADR-004-action-state-machine-and-execution-semantics.md) | Action State Machine and Execution Semantics | Accepted (Rev 2.3) | ✔ (hard gate) |
| [ADR-005](ADR-005-approval-ownership-and-atomic-execution-boundary.md) | Approval Ownership and the Atomic Execution Boundary | Accepted (Rev 2.3) | ✔ |
| [ADR-011](ADR-011-scheduler-fairness.md) | PostgreSQL Fair Scheduler | Accepted (Rev 1.0) | Phase 12 |
| [ADR-012](ADR-012-budget-reservation.md) | Hard Budget Reservation | Accepted (Rev 1.0) | Phase 11 |
| [ADR-014](ADR-014-postgresql-authority-nats-signals.md) | PostgreSQL as Execution Authority; NATS for Signals | Accepted (Rev 1.0) | Phase 10 |
| [ADR-015](ADR-015-dependency-graph.md) | Dependency Graph and Conservative Blast Radius | Accepted (Rev 1.0) | Phase 15 |
| [ADR-016](ADR-016-distributed-kill-switch.md) | PostgreSQL Authority for Distributed Execution Kills | Accepted (Rev 1.0) | Phase 16 |
| [ADR-018](ADR-018-release-and-evaluation.md) | Agent Releases: Evaluation, Replay, Shadow, Canary and Rollback | Accepted (Rev 1.0) | Phase 19 |
| [ADR-022](ADR-022-backpressure-bulkheads-circuit-breakers-retry-budgets.md) | Backpressure, Bulkheads, Circuit Breakers and Retry Budgets | Accepted (Rev 1.0) | Phase 13 |
| [ADR-023](ADR-023-mcp-registry-and-tool-fingerprint.md) | MCP Registry and Tool Fingerprint | Accepted (Rev 1.0) | Phase 14 |
| [ADR-024](ADR-024-fleet-operations.md) | Fleet Operations over the Registry Lifecycle | Accepted (Rev 1.0) | Phase 17 |
| [ADR-025](ADR-025-agent-finops.md) | Agent FinOps: Cost Ingest, Chargeback, Soft Budgets and Alerts | Accepted (Rev 1.0) | Phase 18 |
| [ADR-026](ADR-026-governance-as-code.md) | Governance-as-Code: Bundles, Plans, Change Sets and Drift | Accepted (Rev 1.1) | Phases 20–21 |
| [ADR-027](ADR-027-incidents-and-agent-soc.md) | Incidents and the Agent SOC Read Model | Accepted (Rev 1.0) | Phase 22a |
| [ADR-028](ADR-028-operator-console.md) | The Operator Console | Accepted (Rev 1.0) | Phase 22b |
| [ADR-029](ADR-029-high-availability.md) | High Availability: Replicas Without a Leader | Accepted (Rev 1.1) | Phase 23 |
| [ADR-019](ADR-019-credential-custody.md) | Credential Custody: Providers and Just-in-Time Credentials | Accepted (Rev 1.1) | Phases 24a, 24b |
| [ADR-030](ADR-030-a2a-delegation.md) | Governed A2A Delegation | Accepted (Rev 1.0) | Phase 25a |
| [ADR-031](ADR-031-llm-gateway.md) | The LLM Gateway | Accepted (Rev 1.0) | Phase 25b |
| [ADR-032](ADR-032-mcp-tools-call.md) | Governed MCP `tools/call` | Accepted (Rev 1.0) | Phase 26a |
| [ADR-033](ADR-033-agent-studio-and-runtime-credentials.md) | Agent Studio and the runtime's agent credentials | Proposed (Rev 1.0) | Phase 27-0 |
| [ADR-034](ADR-034-result-channel.md) | The result channel | Accepted (Rev 1.0) | Phase 26b |
| ADR-006 … ADR-021 (others, except 011, 012, 014, 015, 016, 018 and 019) | See MASTER_PLAN §74 | Not started | |

ADR-007 to ADR-010 and ADR-013 must conform to ADR-004.

## Rules

- Where an assumption is still unresolved, choose the option that's **most conservative for correctness and safety**, and record it in the ADR's "Unresolved assumptions" table.
- Never claim upstream API behaviour that hasn't been verified (MASTER_PLAN §107). Mark it for a verification spike instead.
