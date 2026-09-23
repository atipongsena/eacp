# ADR-002: AGT/ACS Integration via Sidecar PDP

- **Status:** Accepted
- **Date:** 2026-09-23
- **Phase 0 gate:** yes
- **Related:** MASTER_PLAN §4, §5, §5.1, §12, §13, §83 (Phase 9); ADR-001, ADR-005
- **Review input:** `docs/reviews/2026-09-23-master-plan-review.md` (C1, X1; Codex disagreement on C1)

## Context

Microsoft Agent Governance Toolkit (AGT) and the Agent Control Specification (ACS) are EACP's intended governance foundation. As of 2026-09-23 the upstream facts are:

| Fact | Source |
|---|---|
| AGT is **Public Preview v4.1.0**, MIT-licensed, and may break before GA | AGT repository |
| ACS is an **in-process, stateless** library: a Rust core with C-ABI, PyO3, napi and P/Invoke bindings, and SDKs for **Python, Node, .NET and Rust**. **There's no Go binding.** | ACS docs |
| The AGT Go SDK covers **core only** (policy, identity, trust, audit). It has **no `require_approval` approval-chain execution layer and no ADR-0030 action binding.** Issue #3083 is open. | AGT repo, issue #3083 |
| ACS verdicts are `allow / warn / deny / escalate / transform`. Action binding uses JCS (RFC 8785) + SHA-256, and approval binds to the post-transform `enforced_identity`. | ACS docs, AGT spec |
| "Revalidation" is not an ACS primitive | ACS docs |

EACP's core is written in Go (§17). Revision 1 assumed a native Go `MicrosoftAGTProvider`, which doesn't exist.

## Decision

### 1. Go is the core; AGT/ACS is behind an interface

All EACP domain code depends only on the Go `GovernanceProvider` interface (§12). No AGT/ACS types cross that boundary.

```go
type GovernanceProvider interface {
    // Evaluate must be side-effect free: no approval state lives in the provider.
    Evaluate(ctx context.Context, req GovernanceRequest) (GovernanceDecision, error)
}
```

`GovernanceDecision` carries: verdict, enforced payload, input and enforced digests, policy bundle ID and version, provider instance ID, decision ID, reasons, approval requirement (for `escalate`), and evaluation time.

### 2. AGT/ACS runs as a sidecar PDP, not through cgo

- `sidecars/agt-pdp/` is a small service that wraps the AGT **Python** SDK, which is the full stack. The AGT version is pinned exactly.
- It exposes one operation, `Evaluate`, over HTTP/JSON on a Unix domain socket or loopback within the same pod or host. mTLS is **required** if it's ever reached over a network.
- `integrations/governance/microsoftagt/` is the Go client that implements `GovernanceProvider`.
- The sidecar is **stateless**, matching ACS. Approval state is owned by EACP (ADR-005).

### 3. `local` provider first

`integrations/governance/local/` is a deterministic Go provider that evaluates versioned policy bundles stored in Postgres. It can return all five verdicts, including `transform`. **Slice A uses only this provider**, so execution correctness never waits on AGT. The AGT sidecar arrives in Slice B (Phase 9).

### 4. EACP computes digests itself

EACP computes `input_digest` and `enforced_digest` in Go (JCS + SHA-256) from the snapshot it sent and the enforced payload it received. If the provider also reports digests and they differ, the decision is treated as **Deny** (reason `digest_mismatch`). EACP never trusts a digest it didn't compute.

### 5. Decision evidence is mandatory

Every decision is persisted with provider, provider instance ID, policy bundle ID, policy version, verdict, reasons, both digests, decision ID and time. A response missing any required field is **Deny** (reason `incomplete_decision`). This is Codex finding X1.

### 6. Failure behaviour depends on the decision type

This follows Codex's refinement of C1.

| Call path | PDP unavailable / timeout / malformed |
|---|---|
| New side-effecting action (submission) | **Deny** (`governance_unavailable`, retryable by the client with the same idempotency key after the outage) |
| Release boundary revalidation (after approval) | **Do not release**: the action stays `AUTHORIZED` and retries until `not_after` → `EXPIRED`. Human approvals aren't wasted by a transient outage, and nothing executes without a fresh decision. |
| Cancel, reconciliation reads, human resolution, containment (kill, suspend) | **Never consult the PDP.** These must work during a PDP outage. |

### 7. Version pinning and compatibility

- The sidecar reports its AGT version and policy bundle version on a health endpoint.
- EACP **refuses to use** a sidecar whose AGT version doesn't match the pinned version (fail closed at startup, with a health alert at runtime).
- **Conformance suite:** a reference policy set evaluated by both `local` and `microsoftagt` must produce identical verdicts and enforced payloads. It runs in CI on every AGT version bump.

## Consequences

**Positive**
- A clean dependency and runtime boundary. A crash or memory bug in native AGT/ACS code can't take down the Go process.
- There's no cgo in the core, so cross-compilation and static binaries stay simple.
- Switching to a future official Go ACS binding, or to OPA, Cedar or a custom PDP, is a new `GovernanceProvider` implementation with no domain changes.
- Using the Python full stack gets the most complete AGT feature set.

**Negative / costs**
- An extra network hop per evaluation, which adds latency. It's measured separately as `governance_latency` (§105).
- A second runtime (Python) to package, patch and monitor.
- Two policy engines (`local` and AGT) can diverge. The conformance suite is the mitigation and is mandatory.

## Unresolved assumptions and conservative defaults

| Unresolved | Conservative default |
|---|---|
| The exact AGT Python API for ACS `Evaluate` (module and function names, snapshot schema) | **Not assumed.** The Phase 9 spike must verify against the pinned AGT release and record the result in `research/REFERENCES.md` before implementation (§107: no invented APIs). |
| Whether AGT's `enforced_identity` digest byte-for-byte matches EACP's JCS implementation | EACP's own digest is authoritative. On any mismatch → Deny (§4 above). |
| Timeout budget for `Evaluate` | Configurable. A timeout is treated as unavailable (table in §6). No SLO until measured. |
| Whether `warn` needs human visibility | `warn` is allowed but recorded as evidence and surfaced in the operator UI later. It never downgrades to allow silently without evidence. |

## Verification

- Unit: the `local` provider returns every verdict type, and `transform` produces an enforced payload and a distinct `enforced_digest`.
- Security: PDP timeout at submission → `DENIED(governance_unavailable)`. PDP down during release → stays `AUTHORIZED`, no dispatch. Kill and cancel work while the PDP is down.
- Security: a digest mismatch or incomplete decision → Deny.
- Slice B: conformance suite green for `local` vs `microsoftagt` on the reference policy set.

## Alternatives considered

| Option | Why not chosen |
|---|---|
| **cgo against the ACS C-ABI** | cgo complicates builds and cross-compilation, a native crash kills the Go process, and a preview ABI has no stability promise. It also couples the core to AGT internals, which §5 forbids. |
| **AGT Go SDK (core) only** | No approval chains or action binding (issue #3083), and no ACS verdict parity. It would force EACP to reimplement AGT semantics in Go. It can be revisited as a provider once parity lands. |
| **Node or .NET sidecar** | Viable, but Python is AGT's most complete stack. The sidecar contract is language-neutral, so this can change later without touching EACP. |
| **Fork AGT** | Rejected by the extension-first strategy (§5). |
