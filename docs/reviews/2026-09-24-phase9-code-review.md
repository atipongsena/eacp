# Phase 9 AGT Sidecar PDP Review

Date: 2026-09-24
Scope: Slice B Phase 9 (MASTER_PLAN §83; ADR-002 Rev 2.4 §8; §103 invariants 10, 14, 18).
Review: self-review against ADR-002 and the plan's invariants I1–I7 (`docs/superpowers/plans/2026-09-24-phase-9-agt-sidecar-pdp.md`), plus mutation checks of the conformance suite.

## Spike (before any code)

ADR-002 required verifying the upstream API before implementation. The results, all checked against the released artifacts by running them, are in `research/REFERENCES.md`.

- **Evaluator.** The ACS-backed evaluator is `agt.policies.runtime.AgtRuntime` in `agt-policies` 5.0.0, over ACS 0.3.1b1 (`agent-control-specification`, an upstream abi3 Linux wheel).
- **Policies.** Rego policies run through the OPA CLI.
- **Transforms.** An ACS transform is a single-path replacement, and an `escalate` verdict carries no transform.
- **Runtime errors.** A runtime error fails to `deny` with a `runtime_error:*` reason.
- **Identity.** The ACS action identity is sorted serde_json over the whole ACS policy input. It is not JCS. In 0.3.1b1 Python reports only the enforced identity.
- **Packaging.** The meta package `agent-governance-toolkit` 4.1.0 does not contain ACS. The ACS sdist ships a stale `Cargo.lock` (`--locked` fails), so the sidecar installs the hash-pinned upstream wheel.

## Implemented

- **Sidecar `sidecars/agt-pdp` (Python 3.12, stateless).**
  - Wire protocol `eacp-agt-pdp/1`: `POST /v1/evaluate` and `GET /healthz`.
  - A fixed Rego adapter evaluates EACP format-1 bundles, supplied as OPA data, inside ACS at `pre_tool_call`. It uses first-match semantics, shallow replacement of the whole `$policy_target`, and the matched rule id in the verdict evidence.
  - It uses `AgentControl.evaluate_intervention_point` only, never `enforce()` or `run()`, so no approval is ever resolved.
  - `decide()` rejects any ACS output that disagrees with the bundle: unknown rule, different verdict or reason, a transform that isn't a whole-object replacement, or a non-default deny without a rule.
  - It computes EACP's JCS digests itself with `rfc8785`.
  - JSON parsing is strict: duplicate keys, NaN, 1e400, lone surrogates and excessive depth are rejected.
  - Bundle validation matches the local provider. Escalate with `set` is `policy_unsupported`.
  - The ACS runtime cache is bounded and keyed by content.
  - Transport: loopback plain HTTP or mutual TLS 1.3 only. Bodies and concurrency are bounded. The TLS handshake runs in the request thread.
  - Every input is pinned: Python packages by hash, OPA by checksum, and a UID shared with the EACP images.
- **Go client `integrations/governance/microsoftagt`.**
  - Implements `GovernanceProvider`.
  - Transport: plain `http` to loopback hosts only, `https` with mTLS 1.3 (all three files required), no proxy, no redirects.
  - Response handling: a 2 MiB cap, strict decoding (unknown fields rejected), protocol and version pins checked on **every** decision, and `evaluated_at` within 5 s of the call window.
  - Error classes: a non-200 is unavailable; a bad 200 is malformed, and the engine alerts.
  - `WriteDevPKI` and `eacpctl pdp-dev-certs` issue the development PKI; the command is refused outside development and test.
- **Wiring.**
  - `EACP_GOVERNANCE_PROVIDER=local|microsoft-agt`, plus `EACP_AGT_PDP_*`.
  - At `controlplane-api` startup, a reachable sidecar with other versions aborts; an unreachable one only warns. The PDP is deliberately not a readiness check, because cancel and containment must survive its outage (ADR-002 §6).
- **Evidence.** Migration 00009 adds `decision_evidence.provider_evidence`: a JSON object, at most 4 KiB canonical in Go and 8 KiB rendered in SQL. It holds the ACS identity, the rule, the adapter digest and the versions, and is journaled with the decision and shown in the evidence report.
- **Conformance.** `test/conformance/governance_reference.json` has 17 cases, covering every verdict, first-match, wildcards, shallow transforms, Unicode ordering, number canonicalization, failures, and the documented divergence. It holds EACP's digests and is checked by three suites:
  - the local provider (`internal/governance`);
  - the wire protocol (`microsoftagt`);
  - the real sidecar through ACS and OPA, in `docker build --target test`.

  The digests therefore agree across Go JCS and Python `rfc8785`.
- **Compose.** Four pieces:
  - `pdp-pki`, a one-shot service;
  - `agt-pdp` on an internal `pdp` network;
  - `controlplane-api` switched to `microsoft-agt`;
  - security tests: mTLS answers with a client certificate, the handshake is refused without one, the agent has no route, and only the API and the sidecar mount the PKI.
- **Demo.** Every decision in the demo is now evaluated by the AGT sidecar. The new step 11 stops the sidecar:
  - Submissions get 503 and stay `RECEIVED`.
  - Cancel works without the PDP.
  - After a restart, the sweeper evaluates the waiting action, which executes once.

  The evidence step asserts the AGT provenance.

## Review findings and fixes

1. **`object.union` is a deep merge.** Using it alone would have diverged from the local provider's shallow replacement for nested objects. The adapter first removes the replaced keys (`object.remove`).
   - Mutation check: dropping `object.remove` fails `transform_is_shallow`, both in the conformance run and under concurrency.
2. **An ACS runtime error would have been a terminal denial.** ACS maps OPA failures, limit breaches and invalid output to `deny`. Recording that verdict would turn a PDP outage into `DENIED`, contradicting ADR-002 §6. `runtime_error:*` is now 503, a transient failure.
   - Tests: OPA missing, over-deep payload, and a non-object payload with `set`.
3. **Escalate with `set` can't be expressed in ACS.** Evaluating it anyway would have approved a payload different from the one the local provider binds. The sidecar refuses the bundle with `policy_unsupported`; the action fails closed. This is recorded as a documented divergence, and the reference set checks it.
4. **Mismatch on digests EACP didn't compute.** The ACS identity covers a different document and a different canonical form. EACP never compares it; it is evidence only. The sidecar computes EACP's own JCS digests, so `EvaluateChecked`'s comparison is a real cross-implementation check.
5. **A response cap of 1 MiB was too small.** A non-transform decision echoes a payload of up to 1 MiB, which would have been classed malformed. The cap is now 2 MiB. ACS's own limits (depth 64, a 1 MiB snapshot, 256 KiB of policy output) fail closed with 503 and are documented as a divergence.
6. **A skewed PDP clock could stretch an approval TTL,** which counts from `evaluated_at`. The client bounds it to the call window ±5 s.
7. **Reference-set digests.** The reference set's first digest was a placeholder. All digests are now generated by `-update` from the local provider and reproduced independently by the sidecar.

## Accepted residual risks

- The development PKI volume is shared, so the sidecar can read the client key and the API the server key. This is development only; production issues separate credentials from its own CA.
- A tampered Rego adapter that returns the default deny while a rule matched cannot be detected per decision. The conformance suite and the adapter digest in the evidence cover it.
- Latency is about 23 ms per decision (p50 and p95), dominated by the OPA process per call. That is measured, not an SLO. An in-process Rego engine is the follow-up if Phase 12/13 capacity needs it.
- The sidecar's Python suites run in its image build, not in `go test ./...`. `AGENTS.md` lists the command.

## Verification

- `go vet ./...` and `go test -race ./...` with `EACP_TEST_ADMIN_DSN`: all 28 packages pass.
- `docker build -f sidecars/agt-pdp/Dockerfile --target test .`: 25 tests pass, including the full reference set through ACS and OPA.
- `EACP_COMPOSE_TEST=1 go test ./test/security/`: 12 tests pass, 4 of them new.
- `scripts/demo.sh`: passes in 71 s, all 13 steps through the AGT sidecar.
