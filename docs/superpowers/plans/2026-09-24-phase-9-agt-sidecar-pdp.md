# Phase 9 — AGT Sidecar PDP (Slice B) — plan

Normative: MASTER_PLAN §83, ADR-002 Rev 2.4 §8, and `research/REFERENCES.md`.
This phase does not change the approval store or the action core.

## Invariants this phase must keep

- **I1 (ADR-002 §4, §103-14).** An AGT decision is used only when the sidecar's
  JCS digests equal EACP's. A mismatch means `DENIED(digest_mismatch)` plus an alert.
- **I2 (§6, §103-18).** An unavailable, slow, malformed, version-mismatched or
  runtime-error PDP never makes an action executable, and never makes it
  terminally `DENIED`. The action stays `RECEIVED`, the API answers 503, and
  release stays `AUTHORIZED`. Cancel and containment never call the PDP.
- **I3 (ADR-005).** The sidecar holds no approval state and never resolves an
  escalation. `escalate` carries the approval requirement of the matched rule,
  checked against the ACS decision.
- **I4 (§7).** The sidecar runs only the pinned versions: AGT policies 5.0.0,
  ACS 0.3.1b1 and OPA 1.20.2. It reports them, and every decision is checked
  against the pins.
- **I5 (conformance).** `local` and the AGT sidecar give identical verdicts,
  reasons, enforced payloads, approval requirements and digests on the reference
  set. The documented divergences fail closed.
- **I6 (transport).** The sidecar is reached only over loopback or mutual TLS
  1.3. No proxy, no redirects, bounded bodies.
- **I7 (evidence).** Every AGT decision persists its provider evidence: the ACS
  identity, the matched rule, the adapter digest and the versions.

## Tasks (tests first)

1. Reference set `test/conformance/governance_reference.json`, and
   `internal/governance/conformance_test.go` for `local` against it.
2. Migration 00009 adds `decision_evidence.provider_evidence`.
   `GovernanceDecision.ProviderEvidence` is checked in `EvaluateChecked` and
   stored by `RecordDecision`. Add it to the RLS and down-migration tests.
3. Sidecar `sidecars/agt-pdp`:
   - bundle validation and mapping
   - the Rego adapter
   - the ACS evaluation
   - the digests
   - the HTTP/mTLS server
   - Python `unittest` suites, including the conformance runner
   - a Dockerfile whose `test` stage runs them, with hash-pinned requirements
4. Go client `integrations/governance/microsoftagt`. Its `httptest` suites
   cover the happy path, every failure class, version pins, clock skew, mTLS
   and loopback-only plain HTTP.
5. Config (`EACP_GOVERNANCE_PROVIDER`, `EACP_AGT_PDP_*`), the startup version
   check in `controlplane-api`, and `eacpctl pdp-dev-certs`.
6. Compose: an `agt-pdp` service on an internal `pdp` network with mTLS, and
   `controlplane-api` switched to `microsoft-agt`. The security tests cover the
   verdicts through the live path, PDP outage and recovery, and cancel during
   an outage. The demo runs through AGT.
7. Docs: `INVARIANTS.md`, `AGENTS.md`, `DEMO.md`, the README, and the phase
   review.
