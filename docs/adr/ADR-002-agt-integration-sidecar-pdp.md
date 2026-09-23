# ADR-002: AGT/ACS Integration via Sidecar PDP

- **Status:** Accepted — Rev 2.4 (Phase 9 sidecar contract, 2026-09-24; Rev 2.3 Phase 4 action integration; Rev 2.2 Phase 3 local-provider contract)
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

`internal/governance.LocalProvider` is a deterministic Go provider that evaluates immutable, versioned policy bundles stored in Postgres. It can return all five verdicts, including `transform`. **Slice A uses only this provider**, so execution correctness never waits on AGT. The AGT sidecar arrives in Slice B (Phase 9).

The Phase 3 bundle format is JSON with `format_version: 1` and a nonempty ordered `rules` array. Each rule has a unique `id`, a nonblank `reason`, a `verdict`, and optional exact-match fields (`subject`, `operation`, `target`, `tool`, `risk_class`, `side_effect_class`). The first matching rule wins; no match returns `deny` with `no_matching_rule`. `transform` requires a nonempty top-level JSON object `set`; `escalate` may apply the same replacements before approval and requires `approval` with quorum 1–5, `eligible_roles: ["approver"]`, and TTL 1–86400 seconds. Numeric replacements are limited to magnitude 2^53 because PostgreSQL `jsonb` can expand exponent notation when a bundle is reloaded; larger amounts must be strings. Unknown fields and malformed input are rejected. Policy insertion validates the format in Go and PostgreSQL so raw application-role SQL cannot activate a bundle the local provider cannot parse.

The governance input is built by EACP, never by the agent. Phase 4 builds it as follows:
- `tenant`, `agent_id` and `agent_version_id` come from the authenticated key.
- `subject` is the value the agent asserts. It must name an enabled human principal of the tenant, or the action is `DENIED(subject_invalid)`. The assertion is not proof of the user's consent; on-behalf-of proof is a later ADR.
- `risk_class` is the agent's risk class.
- `side_effect_class` is the active contract's side effects, sorted and joined by `,` (for example `FINANCIAL,IRREVERSIBLE_WRITE`), matched exactly.

### 4. EACP computes digests itself

EACP computes `input_digest` and `enforced_digest` in Go (JCS + SHA-256) from the snapshot it sent and the enforced payload it received. If the provider also reports digests and they differ, the decision is treated as **Deny** (reason `digest_mismatch`) and a security alert is raised. This is deterministic and terminal. EACP never trusts a digest it didn't compute.

Phase 3 implements the digest check and deny verdict in `EvaluateChecked`. Phase 4 (`internal/action`) raises the alerts. They are structured `security alert` log records with `alert` set to one of two values:
- `governance.digest_mismatch`: the action is `DENIED(digest_mismatch)`.
- `governance.malformed_decision`: the action stays `RECEIVED`, per §5.

Metrics and alert routing arrive with the telemetry work (MASTER_PLAN §105). The JCS parser rejects duplicate keys, invalid Unicode and integer tokens outside the interoperable I-JSON range; callers must use JSON strings for larger identifiers or amounts.

### 5. Decision evidence is mandatory

Every decision is persisted with provider, provider instance ID, policy bundle ID, policy version, verdict, reasons, both digests, decision ID and time. A response missing any required field is treated as a **transient failure**: nothing becomes executable, the same handling as an unavailable PDP (§6), plus an alert. This is Codex finding X1.

### 6. Failure behaviour depends on the decision type

This follows Codex's refinement of C1.

| Call path | PDP unavailable / timeout / malformed |
|---|---|
| New side-effecting action (submission) | **Nothing becomes executable.** The action stays `RECEIVED` (ADR-004 T2a) and the API returns **503 retryable** with `action_id`. A resubmit with the same `Idempotency-Key`, or the sweeper, re-evaluates it after recovery. The action is never made terminally `DENIED` because of an outage. |
| Release boundary revalidation (after approval) | **Do not release**: the action stays `AUTHORIZED` and retries until `not_after` → `EXPIRED`. Human approvals aren't wasted by a transient outage, and nothing executes without a fresh decision. |
| Cancel, reconciliation reads, human resolution, containment (kill, suspend) | **Never consult the PDP.** These must work during a PDP outage. |

### 7. Version pinning and compatibility

- The sidecar reports its AGT version and policy bundle version on a health endpoint.
- EACP **refuses to use** a sidecar whose AGT version doesn't match the pinned version (fail closed at startup, with a health alert at runtime).
- **Conformance suite:** a reference policy set evaluated by both `local` and `microsoftagt` must produce identical verdicts and enforced payloads. It runs in CI on every AGT version bump.

### 8. The Phase 9 sidecar contract (Rev 2.4)

The spike verified the upstream API by running it; the results are in `research/REFERENCES.md`. The findings that shape this section:
- The ACS-backed evaluator is `agt.policies.runtime.AgtRuntime` in **`agt-policies` 5.0.0**. It wraps **ACS 0.3.1b1**, a Rust core with an upstream abi3 Linux wheel.
- ACS Rego policies run through the **OPA CLI**.
- The meta package `agent-governance-toolkit` 4.1.0 does not contain ACS.

**Pins.** The sidecar pins these three together. The Go client refuses any other set (§7):
- `agt-policies==5.0.0`
- `agent-control-specification==0.3.1b1`, the upstream Linux wheel, pinned by hash
- OPA v1.20.2, a checksum-pinned static binary

The sidecar reports all three on `GET /healthz` and on every decision. The Go client checks them:
- **At startup**, a reachable sidecar with the wrong versions aborts `controlplane-api` (fail closed). An unreachable one only logs, so the API still starts: cancel, reconciliation reads and containment must work during a PDP outage (§6).
- **On every decision**, a version or protocol mismatch is a malformed decision. The action stays `RECEIVED` and the `governance.malformed_decision` alert fires. For the same reason as startup, the PDP is **not** a `/readyz` check: an outage must not take the API out of service.

**Policy mapping.** The sidecar is stateless. Each request carries the policy bundle EACP pinned: id, version and content. The sidecar caches one ACS runtime per `(bundle id, version, SHA-256 of the content)`, in a bounded LRU.

A fixed Rego adapter module (`eacp.pdp`) evaluates the bundle's rules, supplied as OPA data, inside ACS at the `pre_tool_call` intervention point:
- `policy_target` is `$snap.tool_call.args`, which holds the submitted payload.
- `snapshot.eacp` holds the binding fields, `risk_class` and `side_effect_class`.
- The first matching rule wins. An empty-string match field is a wildcard, as in `local`. No match gives `deny` / `no_matching_rule`.
- A multi-key `set` becomes one ACS transform of `$policy_target`: the payload minus the replaced keys, united with `set`. That is a shallow replacement, as in `local`.
- The matched rule id travels in `evidence.verification_pointers.eacp_rule_id`. For `escalate`, the sidecar takes quorum, roles and TTL from that rule. It first checks that the rule's verdict equals the ACS decision; if not, the decision is malformed.

The sidecar never calls an approval resolver. It calls `AgentControl.evaluate_intervention_point` in `enforce` mode and never calls `enforce()` or `run()`, so an `escalate` comes back as a verdict and is never auto-resolved. Approval state stays in EACP (ADR-005).

**Divergences from `local`, all failing closed:**

| Case | `local` | `microsoft-agt` |
|---|---|---|
| `escalate` with `set` | payload rewritten, then approval | ACS can't carry a transform on `escalate`. The sidecar refuses the bundle (`422 policy_unsupported`), and the action stays `RECEIVED`. |
| ACS `runtime_error:*` (OPA missing or slow, invalid policy output, limits) | — | Reported as `503 pdp_runtime_error`, a transient failure. It is **never** a terminal `DENIED`. |
| `set` on a non-object payload | provider error | The adapter emits an invalid decision, which becomes `runtime_error:policy_output_invalid` and then 503. The action stays `RECEIVED` either way. |
| A payload beyond ACS's resource limits (policy-input depth 64, 1 MiB snapshot, 256 KiB policy output) | evaluated | ACS reports `runtime_error:*`, which becomes 503. The action stays `RECEIVED` until `not_after`. |

**Digests.**
- The sidecar computes EACP's two JCS digests itself, with `rfc8785`. EACP compares them with its own (§4). This is a real cross-implementation check.
- ACS's `action_identity` (sorted serde_json over the ACS policy input, not JCS) is kept only as provider evidence. In ACS 0.3.1b1 the Python binding reports only the enforced identity.

**Provider evidence.** Migration 00009 adds `decision_evidence.provider_evidence`. It is a nullable JSON object of at most 4 KiB. For AGT it holds the ACS identity, the matched rule id, the adapter module's digest and the three versions. `local` leaves it null.

**Wire protocol `eacp-agt-pdp/1`.**
- `POST /v1/evaluate` takes the binding, `risk_class`, `side_effect_class` and the policy.
- A 200 response returns every field of `GovernanceDecision`. Every other status is a transient failure: 400, 413, 422, 503 or 500.
- The Go client rejects:
  - unknown fields;
  - a body over 2 MiB;
  - redirects;
  - an `evaluated_at` more than 5 s outside the call window.
- The client ignores proxy environment variables.

**Transport.**
- `EACP_GOVERNANCE_PROVIDER=local|microsoft-agt`. The default is `local`.
- `EACP_AGT_PDP_URL` is either:
  - `http://` to a loopback host only, or
  - `https://` with mutual TLS 1.3, which requires `EACP_AGT_PDP_CA_FILE`, `EACP_AGT_PDP_CERT_FILE` and `EACP_AGT_PDP_KEY_FILE`.
- The sidecar either requires a client certificate or refuses to bind a non-loopback address.
- Docker Compose runs it on an internal `pdp` network that only `controlplane-api` shares. It uses mTLS with a development PKI that `eacpctl pdp-dev-certs` writes into a named volume.

**Health and latency.** `GET /healthz` reports the protocol, the three versions and the instance. It reports no policy version, because the sidecar holds no policy (§7's policy-version bullet is superseded by the per-request bundle).

Measured in the image, one decision takes about 23 ms at p50 and p95. Most of that is the OPA process ACS spawns per call; a new bundle adds about 4 ms. This is a measurement, not an SLO (§105). An in-process Rego engine is the follow-up if Phase 12/13 capacity needs it.

**Conformance.** `test/conformance/governance_reference.json` records each case's expected outcome and EACP's digests. Three suites run against it:
- `internal/governance` checks `local` against it.
- The sidecar image's test stage checks the AGT sidecar against it, through ACS and OPA.
- The Compose security test checks the live path through the Go client.

So a digest or verdict divergence fails the build.

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
| The exact AGT Python API for ACS `Evaluate` (module and function names, snapshot schema) | **Resolved in Rev 2.4** by the Phase 9 spike (`research/REFERENCES.md`), which ran the pinned release. |
| Whether a later ACS reports a distinct `input_identity` from Python | EACP stores whatever ACS reports as evidence only, and never relies on it. |
| An AGT/ACS release that changes the Rego adapter's semantics | The version pins, and the conformance suite in the image build, refuse it until the suite passes again. |
| Whether AGT's `enforced_identity` digest byte-for-byte matches EACP's JCS implementation | EACP's own digest is authoritative. On any mismatch → Deny (§4 above). |
| Timeout budget for `Evaluate` | Configurable (`EACP_PDP_TIMEOUT`, default 5s, at most 1 minute). A timeout is treated as unavailable (table in §6). No SLO until measured. |
| Whether `warn` needs human visibility | `warn` is allowed but recorded as evidence and surfaced in the operator UI later. It never downgrades to allow silently without evidence. |

## Verification

- Unit: the `local` provider returns every verdict type, and `transform` produces an enforced payload and a distinct `enforced_digest`.
- Security: PDP timeout at submission → `RECEIVED` + 503. After the PDP recovers, a resubmit with the same key is evaluated normally. PDP down during release → stays `AUTHORIZED`, no dispatch. Kill and cancel work while the PDP is down.
- Security: a digest mismatch → `DENIED(digest_mismatch)` plus an alert. An incomplete decision → stays `RECEIVED` plus an alert.
- Slice B: conformance suite green for `local` vs `microsoftagt` on the reference policy set.

## Alternatives considered

| Option | Why not chosen |
|---|---|
| **cgo against the ACS C-ABI** | cgo complicates builds and cross-compilation, a native crash kills the Go process, and a preview ABI has no stability promise. It also couples the core to AGT internals, which §5 forbids. |
| **AGT Go SDK (core) only** | No approval chains or action binding (issue #3083), and no ACS verdict parity. It would force EACP to reimplement AGT semantics in Go. It can be revisited as a provider once parity lands. |
| **Node or .NET sidecar** | Viable, but Python is AGT's most complete stack. The sidecar contract is language-neutral, so this can change later without touching EACP. |
| **Fork AGT** | Rejected by the extension-first strategy (§5). |
