# Upstream references (verified)

Facts here were checked against the released artifacts, not the docs alone
(MASTER_PLAN §107: no invented APIs). Re-verify on every version bump.

## Phase 9 spike — AGT / ACS Python API (2026-09-24)

### Pinned artifacts

| Artifact | Version | Source | SHA-256 |
|---|---|---|---|
| `agt-policies` (AGT 5.0 policy layer, package `agt`) | 5.0.0 | PyPI wheel | `02afdc295ab7f0aff4f40346f3da31101ee4c635a4df33aee45d03035d784bfc` |
| `agent-control-specification` (ACS Python SDK + Rust core) | 0.3.1b1 | PyPI wheel `cp311-abi3-manylinux_2_28_x86_64` (a pre-release: `pip index --pre`) | `a0eea57016fc4cf3c620d92b81ad8c7b1376aa2865991a561ea6a427cb1451be` |
| OPA CLI (the ACS Rego dispatcher shells out to it) | v1.20.2 | GitHub release `opa_linux_amd64_static` | `69da5179ee403d10fa11bab6cfb4ffb0d23dba5f9b682fa977db772a1da5670f` |
| `rfc8785` (JCS for the sidecar's own digests) | 0.1.4 | PyPI wheel | pinned by hash in `sidecars/agt-pdp/requirements.txt` |

There's no Windows wheel, so `pip` on a Windows host falls back to the sdist
(`560a717b…`). That sdist's `Cargo.lock` is stale: `cargo --locked` refuses it.
The sidecar therefore installs the upstream Linux wheel by hash. Its `glibc`
floor is 2.28; Debian bookworm has 2.36.

The meta package `agent-governance-toolkit` is still 4.1.0 and does **not** contain
ACS. In 4.1.0 the v4 adapters reach ACS only through
`agent_os.integrations._v5_runtime_bridge`, which imports `agt.policies.runtime`
from `agt-policies` 5.0.0. `agent-governance-toolkit-core` 5.0.0 also exists.
EACP depends only on `agt-policies` 5.0.0 and ACS 0.3.1b1.

### API verified by running it (image `sidecars/agt-pdp`, Python 3.12)

- `agt.policies.runtime.AgtRuntime(manifest_path)` loads an ACS manifest.
  `AgtRuntime.control` is the ACS `agent_control_specification.AgentControl`.
- `await AgentControl.evaluate_intervention_point(ip, snapshot, mode)` returns
  `InterventionPointResult(verdict, transformed_policy_target,
  transformed_policy_target_applied, policy_input, input_identity,
  enforced_identity)`. It does **not** consult an approval resolver; only
  `AgentControl.enforce()` / `run()` and `AgtRuntime.evaluate_intervention_point`
  in `enforce` mode do. With no resolver those turn `escalate` into a block
  (`deny`), so the sidecar must never call them.
- `Verdict(decision, reason, message, transform, evidence, result_labels)`.
  Decisions: `allow | deny | warn | escalate | transform`.
- A `transform` is a **single-path** replacement `{path, value}` rooted at
  `$policy_target`. The path `$policy_target` itself is accepted, and it replaces
  the whole target. In `enforce` mode the core applies it
  (`transformed_policy_target`). In `evaluate_only` mode the core validates it
  but does not apply it.
- A transform is forbidden on every other decision. An `escalate` verdict never
  carries or applies a transformed target.
- Verdict evidence is `{artefact?: str, verification_pointers?: {str: str}}`,
  capped at 4 KiB serialized. It is propagated verbatim.
- Every runtime failure (OPA missing or timing out, invalid policy output, limits)
  becomes `deny` with a reserved `runtime_error:*` reason. A policy may not emit
  that prefix itself.
- Identity: `action_identity = "sha256:" + hex(SHA-256(serde_json with keys sorted
  recursively))` over the **whole ACS policy input**
  (`intervention_point`, `policy_target`, `snapshot`, `annotations`, `tool`).
  This is **not RFC 8785**: numbers and key order follow serde_json, and keys are
  sorted by UTF-8 bytes, not by UTF-16 code units. It also covers a different
  document from EACP's binding.
  - ACS 0.3.1b1's pyo3 binding emits only the legacy `action_identity`, which
    is the enforced identity. The Python SDK copies it into both
    `input_identity` and `enforced_identity`, so the two are always equal in
    Python.
- Rego policies run through the bundled OPA dispatcher:
  `opa eval --format json --stdin-input --bundle <dir> <query>`.
  - The executable comes from `ACS_OPA_PATH` (default `opa` on `PATH`).
  - The timeout comes from `ACS_OPA_TIMEOUT_MS` (default 5 s).
  - The manifest declares it as `policies.<id>: {type: rego, bundle: <dir>,
    query: data.<pkg>.verdict}`. A bundle directory needs no `.manifest`.
- Snapshot: `pre_tool_call` with `policy_target: "$snap.tool_call.args"` works
  without `tool_name_from` or a `tools:` section.

### Consequences recorded in ADR-002 Rev 2.4

1. EACP still computes its own JCS digests. The sidecar computes the same
   digests with `rfc8785`, and EACP compares them (ADR-002 §4). The ACS
   identity is kept as provider evidence only.
2. EACP's multi-key `set` becomes one ACS transform of `$policy_target`: the
   original object, minus the replaced keys, united with `set`. The replacement
   is shallow; `object.union` alone would merge nested objects.
3. `escalate` with `set` can't be expressed in ACS. The AGT provider refuses such
   a bundle (`policy_unsupported`), and the action fails closed.
4. `runtime_error:*` is an outage, not a policy decision. The sidecar reports
   it as a transient failure (503), so the action stays `RECEIVED`
   (ADR-002 §6). It is never recorded as a terminal `DENIED`.
5. `escalate` carries no quorum or TTL. The Rego adapter puts the matched rule id
   in `evidence.verification_pointers.eacp_rule_id`. The sidecar then takes the
   approval requirement from that rule, after checking that the rule's
   verdict equals the ACS decision.
