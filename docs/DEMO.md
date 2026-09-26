# Slice A, Slice C and JIT credential demos

Three demos run against one isolated stack, each in its own tenant: Slice A (tenant Acme), Slice C (tenant Globex, [below](#slice-c-demo)) and JIT credentials (tenant Umbrella, [below](#jit-credential-demo)).

## Slice A demo

The demo proves the Slice A goal statement (MASTER_PLAN §110) against a running stack:

> Privileged agent actions cannot bypass the control plane, approvals are durable, retries cannot casually duplicate irreversible side effects, and ambiguous execution outcomes are handled explicitly rather than guessed.

It follows the §111 script and checks each claim, so it is also a test (`test/demo`, `TestSliceADemo`).

Since Phase 9 the stack takes its governance decisions from the **Microsoft AGT/ACS sidecar PDP** (`agt-pdp`, ADR-002 §8), reached over mutual TLS. Every decision in the demo is evaluated by the pinned AGT 5.0.0 policy layer, the ACS 0.3.1b1 engine and OPA 1.20.2.

Since Phase 10 the API relays the transactional outbox to **NATS JetStream** (ADR-014). Work hints wake the worker, and action events feed the dashboard stream. PostgreSQL remains the only authority.

Since Phase 11 a connector contract can declare a cost, and the release reserves it on the agent's **hard budget** (ADR-012). Step 13 races 100 purchases against one budget.

## Run it

Requirements: Docker with Compose v2.24 or later, Go and Python 3.

```bash
scripts/demo.sh
```

The script:
1. prepares the local Fake ERP and Fake MCP credentials and the Fake ERP's OAuth client secret;
2. starts a **fresh, isolated** stack as the compose project `eacp-demo` (API on `127.0.0.1:18080`, PostgreSQL on `127.0.0.1:55433`, with its own volumes);
3. runs every demo (`DEMO` picks some: letters from `A`, `C` and `J`, e.g. `DEMO=J`);
4. removes the demo stack and its volumes.

A development stack (project `eacp`, port 8080) is not touched. To keep the demo stack for exploring afterwards, run `KEEP=1 scripts/demo.sh`. The two demos take about two minutes after the images are built. The first build also pulls the sidecar's pinned Python packages and the OPA binary.

`deployments/demo/compose.demo.yml` shortens two timings so the failure scenarios finish quickly: a 10-second worker lease, and three reconciliation lookups before a human is asked.

### What it shows

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
| 11 | Stop the AGT sidecar. Two purchases answer 503 `governance_unavailable` and stay `RECEIVED`. One is cancelled without the PDP. Restart the sidecar; the sweeper evaluates the other, which executes | PDP outage fails closed, and never blocks cancellation (inv. 18, ADR-002 §6) |
| 12 | NATS stays up while the relay publishes every outbox row as work hints and dashboard events. Then stop NATS: a purchase still executes, exactly once, because the worker polls PostgreSQL, and its outbox rows wait. Restart NATS; the relay publishes them | NATS carries hints only; correctness does not depend on it (ADR-014, §60) |
| 13 | `create_po` gets a costed contract version (the payload's amount in THB; erin proposes, rita activates). Alice gives procurement-bot a 3 700 THB budget; she cannot approve her own raise, so bob does. The agent submits 100 purchases of 100 THB at once | Exactly 37 execute, one PO each; 63 are `DENIED budget_exceeded` and never reach the ERP; the account ends at its limit (inv. 3, ADR-012) |
| 14 | Auditor audra reconstructs the high-value purchase from its `action_id`: governance, both votes, the grant, the attempt, every journaled move, and a verified hash chain. Each decision names `microsoft-agt`, the pinned AGT/ACS/OPA versions and the matched rule | Evidence reconstruction (inv. 10, 17) |
| 15 | Search every API response, every service log (including the sidecar's and NATS's) and a database dump for the ERP and MCP credentials | Not found anywhere (inv. 11) |

### Output

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

## Slice C demo

`TestSliceCDemo` follows the §111 Slice C script: trigger MCP drift, show the blast radius, kill the affected agent version, show trace and audit evidence. It adds one service to the stack, `fakemcp`: a stateless MCP server (modern revision `2026-07-28`, Streamable HTTP) on the worker-only `erp` network. Its bearer token is held by the worker's connector-secret manifest; the server holds only a verifier. It lists its tools from `/data/tools.json`. The demo replaces that file with `docker compose cp` to play a vendor release.

| Step | What happens | What it proves |
|---|---|---|
| C0 | `eacpctl tenant create` for Globex with admins alice and bob; every person, grant and key is approved by the second admin; a policy allows routine ERP work | Two-person administration, in a second tenant on the same stack |
| C1 | Erin registers the MCP connector `sap-mcp`. Declaring a tool by hand is refused (409). The worker's scanner discovers `get_po`: definition #1, risk `initial`, read-only, with a fingerprint computed by PostgreSQL | Tools are discovered, never declared (ADR-023) |
| C2 | Erin certifies `get_po` as `READ_ONLY`, pinned to definition #1, and rita activates it. `po-assistant` (team procurement) may call `erp.create_po` and `sap-mcp.get_po`; `invoice-bot` (team finance) only `erp.create_po`. A routine purchase executes | One PO in the ERP |
| C3 | The server now lists `get_po` with a new description, an `approve` argument and `destructiveHint: true`. Operator otto requests a rescan. Definition #2 is `high` risk; the contract no longer matches the fingerprint and the tool is quarantined. po-assistant's lookup is `DENIED tool_quarantined` | MCP drift is detected and contained before governance (ADR-023 §6–7) |
| C4 | Blast radius of `sap-mcp`: po-assistant is confirmed, team procurement is affected, invoice-bot is not; coverage `observed_only` | Blast radius from capability edges (ADR-015) |
| C5 | Otto kills po-assistant's version (`security_incident`). Its next purchase uses `erp.create_po`, which did not drift: it stays `QUEUED`, is never attempted and reaches no ERP. invoice-bot keeps working. Otto cannot clear their own kill (403). The held action is cancelled | Kill fencing in PostgreSQL, two-person clear, cancellation never blocked (ADR-016) |
| C6 | The held action and its outbox events carry the agent's W3C trace context. The journal since the drift shows the rescan request, the quarantine (`tools.update` by the scanner), the new definition, the scan, the denied lookup, the kill and the cancellation. Auditor audra verifies the tenant's hash chain | Trace and audit evidence |
| C7 | Otto sees the evaluator's MCP drift incident (critical, po-assistant affected) and the kill incident, acknowledges the drift and links his kill, and cannot resolve it himself; opal resolves it. Audra reads the SOC summary (ADR-027) | Incidents observe; resolving a critical incident takes two people |
| C8 | Erin plans a bundle declaring a `ledger` connector and a `ledger-bot` agent (the plan writes nothing), submits it, cannot approve it herself; rita approves. A replan finds no changes and drift is in sync. Alice then declares dana (auditor) and a funded `ledger` budget; she cannot approve, bob does (ADR-026 Rev 1.1) | Two-person Governance-as-Code for the registry, people and budgets |
| C9 | Search every API response, every service log (including `fakemcp`'s) and a database dump for the ERP and MCP credentials | Not found anywhere |

```text
=== C3. Trigger MCP drift: the server now advertises a different get_po
    fakemcp now lists get_po with a new description, an "approve" argument and destructiveHint: true
    otto requests a rescan: "vendor released sap-mcp 2.1"
    definition #2: risk high, changed [annotations description inputSchema], destructive true
    get_po: contract no longer matches its fingerprint; quarantined (definition 2 changed: annotations, description, inputSchema); executable false
    po-assistant asks for sap-mcp.get_po: DENIED tool_quarantined, before governance
```

No worker calls an MCP tool yet (`tools/call` needs its own ADR). The quarantine therefore shows up as a denial at submission. The kill is shown on an ERP purchase, which is the path a worker can dispatch.

### The operator console

Run the demo with `KEEP=1 DEMO=C scripts/demo.sh` and open `http://127.0.0.1:18080/ui/`. The demo generates its keys in memory and never prints them. To sign in, create your own tenant on the kept stack:

1. Generate two admin keys with `eacpctl key generate --kind principal --tenant <uuid>`.
2. Pass them to `eacpctl tenant create --id <uuid> --admin … --admin …` (see `test/demo` for the flags).
3. Grant yourself `operator` through the API, with a second admin approving the grant.

The walk-through below is what `docs/reviews/2026-09-26-phase22b-console-e2e.md` recorded.

- **Overview.** The overview shows the SOC counters, and the incidents page lists the drift and kill incidents.
- **Containment.** The drift incident shows po-assistant as affected. Its "Pause the agents that use this tool" link opens the fleet form, pre-filled. Preview the operation, confirm it, and link it to the incident.
- **Two-person rules.** The acknowledger cannot resolve the critical incident; a second operator can. The operator who set a kill cannot clear it on the Security page; a second operator can.

## JIT credential demo

`TestJITDemo` shows Phase 24a (ADR-019). Tenant Umbrella's ERP connector names `secret_ref` `fakeerp-jit`. In the worker's connector-secrets manifest that reference is an `oauth2` entry for the Fake ERP's token endpoint (`POST /oauth/token`, client `eacp-worker`, 300-second tokens), not a static credential.

- **J0.** Bootstrap the tenant and a policy that allows routine ERP work.
- **J1.** Register the connector, tools and agent as in Slice A; five purchases each end `SUCCEEDED` with exactly one purchase order.
- **J2.** The Fake ERP audit shows every purchase made by principal `oauth:eacp-worker`, not the static credential. It records each token issuance by the token's SHA-256, never the token.
- **J3.** The secret scan: the OAuth client secret appears in no API response, service log or database dump. Every issued token is also absent: the demo hashes every 43-character window of base64url text in all three and compares against the issued hashes.

The agent cannot reach the token endpoint any more than the ERP (`test/security` `TestAgentCannotReachTheTokenEndpoint`). The JIT demo also runs on Kubernetes (`scripts/k8s-e2e.sh`).

## Scope

The demo credentials, the tenant ids and the Fake ERP and Fake MCP tokens are local-development values (see `deployments/docker/secrets`). The claims hold for conforming deployments only (ADR-001 §3a). The target issues its privileged credential only to the EACP worker, and agents have no network route to it. EACP makes no exactly-once claim: an effect is idempotent where the target supports it, effectively-once where it can be reconciled, and at-most-once where a retry is unsafe (MASTER_PLAN §21).
