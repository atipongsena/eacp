# Architecture Decision Records

These ADRs hold the normative detail behind `docs/MASTER_PLAN.md` (Revision 2). **When an ADR and the master plan disagree, the ADR wins.** Fix the plan to match.

| ADR | Title | Status | Phase 0 gate |
|---|---|---|---|
| [ADR-001](ADR-001-product-boundary-and-enforcement-point.md) | Product Boundary and Enforcement Point | Accepted (Rev 2.1) | ✔ |
| [ADR-002](ADR-002-agt-integration-sidecar-pdp.md) | AGT/ACS Integration via Sidecar PDP | Accepted (Rev 2.3) | ✔ |
| [ADR-003](ADR-003-agent-registry-identity-and-capability.md) | Agent Registry, Identity and Capability Model | Accepted (Rev 1.1) | Phase 2 |
| [ADR-004](ADR-004-action-state-machine-and-execution-semantics.md) | Action State Machine and Execution Semantics | Accepted (Rev 2.2) | ✔ (hard gate) |
| [ADR-005](ADR-005-approval-ownership-and-atomic-execution-boundary.md) | Approval Ownership and the Atomic Execution Boundary | Accepted (Rev 2.3) | ✔ |
| ADR-006 … ADR-021 | See MASTER_PLAN §74 | Not started | |

ADR-007 to ADR-010 and ADR-013 must conform to ADR-004.

## Rules

- Where an assumption is still unresolved, choose the option that's **most conservative for correctness and safety**, and record it in the ADR's "Unresolved assumptions" table.
- Never claim upstream API behaviour that hasn't been verified (MASTER_PLAN §107). Mark it for a verification spike instead.
