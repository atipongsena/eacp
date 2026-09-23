# Slice A demo

The demo proves the Slice A goal statement (MASTER_PLAN §110) against a running stack:

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

It follows the §111 script and checks each claim, so it is also a test (`test/demo`, `TestSliceADemo`).

## Run it

Requirements: Docker with Compose v2.24 or later, Go and Python 3.

```bash
scripts/demo.sh
```

The script:
1. prepares the local Fake ERP credential;
2. starts a **fresh, isolated** stack as the compose project `eacp-demo` (API on `127.0.0.1:18080`, PostgreSQL on `127.0.0.1:55433`, with its own volumes);
3. runs the demo;
4. removes the demo stack and its volumes.

A development stack (project `eacp`, port 8080) is not touched. To keep the demo stack for exploring afterwards, run `KEEP=1 scripts/demo.sh`. The run takes about a minute after the images are built.

`deployments/demo/compose.demo.yml` shortens two timings so the failure scenarios finish quickly: a 10-second worker lease, and three reconciliation lookups before a human is asked.

## What it shows

Everything goes through the public API with keys that each person generated for themselves. Only the tenant's two first admins are bootstrapped, by `eacpctl tenant create` on the owner's break-glass path. The agent's requests come from the agent's key. The ERP is checked independently, through its own audit log on the ERP network.

| Step | What happens | What it proves |
|---|---|---|
| 0 | `eacpctl tenant create` with admins alice and bob. People, role grants and API keys are each proposed by one admin and approved by the other. Two admins activate a policy under which high-value purchases need two approvers. | Two-person administration |
| 1 | Register `procurement-bot` (owner carol): tools with contracts, a version, an allowlist and an agent key, each approved by a second person | Registry, identity, capability (inv. 19) |
| 2 | From the agent container, `fakeerp:8090` does not resolve, `/run/secrets` does not exist, and the control plane answers | No bypass (§3.2, inv. 11) |
| 3 | The agent asks for `erp.cancel_po`, which is not in its allowlist | `DENIED` before governance |
| 4 | A routine purchase is allowed and executed | One PO in the ERP |
| 5 | A high-value purchase escalates. Carol, the subject and the agent's owner, cannot approve it (403); amy approves | Separation of duties (inv. 15) |
| 6 | Restart the API, the worker **and PostgreSQL**. The approval is still pending; ben's vote completes the quorum; the grant is consumed at release | Durable approvals (inv. 2, 16) |
| 7 | Kill the worker while its ERP call is in flight, then start a new one. The journal shows `EXECUTING → UNKNOWN_OUTCOME` ("lease expired during the call") `→ RECONCILING → SUCCEEDED` | No blind re-dispatch; exactly one PO (inv. 1, 4, 12) |
| 8 | Five concurrent submissions with one idempotency key | One action, one PO (inv. 4) |
| 9 | The ERP commits, then the call times out | Reconciled from evidence, not guessed (inv. 5) |
| 10 | Delayed visibility under `BEST_EFFORT`: three "not found" lookups, no retry. Operator otto resolves with evidence | "Not found" is not proof (inv. 6, 13); human resolution |
| 11 | Auditor audra reconstructs the high-value purchase from its `action_id`: governance, both votes, the grant, the attempt, every journaled move, and a verified hash chain | Evidence reconstruction (inv. 10, 17) |
| 12 | Search every API response, every service log and a database dump for the ERP credential | Not found anywhere (inv. 11) |

## Output

The narrative is printed by `go test -v`. For example:

```text
=== 7. Kill the worker mid-dispatch: UNKNOWN_OUTCOME, reconcile, exactly one PO
    62f55a33 → EXECUTING
    execution-worker killed while its call was in flight
    62f55a33 → UNKNOWN_OUTCOME
    62f55a33 → SUCCEEDED
    journaled path: RECEIVED → AUTHORIZED → QUEUED → LEASED → EXECUTING → UNKNOWN_OUTCOME: lease expired during the call → RECONCILING → SUCCEEDED
    ERP audit: exactly one purchase order for eacp:00000000-0000-4000-8000-0000000000d1:62f55a33-…
    reconciliation checks: [found]
```

## Scope

The demo credentials, the tenant id and the Fake ERP token are local-development values (see `deployments/docker/secrets`). The claims hold for conforming deployments only (ADR-001 §3a). The target issues its privileged credential only to the EACP worker, and agents have no network route to it. EACP makes no exactly-once claim: an effect is idempotent where the target supports it, effectively-once where it can be reconciled, and at-most-once where a retry is unsafe (MASTER_PLAN §21).
