[English](README.md) | [ไทย](README.th.md)

# 01: an agent's purchase order, end to end

An AI agent, `procurement-bot`, wants to place a 250,000 THB purchase order in an ERP. It does not hold the ERP's
credentials and cannot reach the ERP at all. It asks EACP instead, and EACP decides, waits for the people who must
agree, executes the order once, and keeps evidence of every step.

This example plays every part with `curl`: the agent, the two approvers (amy and ben) and an operator (otto)
reading the evidence afterwards.

## Run it

You need `curl` and `jq`, the running stack and `examples/setup.sh` done once ([examples/README.md](../README.md)).

```bash
bash examples/01-agent-action/run.sh
```

## What you should see

The action id and the ERP reference change on every run. The rest is the same:

```text
== 1. The agent submits a 250,000 THB purchase order
action e29da5e6-a871-4896-9446-3ea06e045711 is PENDING_APPROVAL: the policy escalates high-value purchases to two approvers

== 2. The agent retries the same request: EACP answers with the same action
same idempotency key → action e29da5e6-a871-4896-9446-3ea06e045711 again, nothing new is created

== 3. Two approvers vote
amy:  request PENDING
ben:  request GRANTED

== 4. EACP releases the action and the worker executes it against the Fake ERP
state: AUTHORIZED
state: SUCCEEDED

== 5. The evidence, as an operator sees it
governance: escalate under policy v1 (a high-value purchase needs two approvers)
governance: escalate under policy v1 (a high-value purchase needs two approvers)
approval: GRANTED, quorum 2, 2 votes, grant consumed by this action: true
attempt 1: succeeded, external reference PO-e29da5e6-a871-4896-9446-3ea06e045711
journal: RECEIVED → PENDING_APPROVAL → AUTHORIZED → QUEUED → LEASED → EXECUTING → SUCCEEDED
audit chain: 100 entries, verified: true

Example 01 passed.
```

## What happened

1. **Submit.** The agent sends `POST /v1/actions` with its own EACP key and an `Idempotency-Key` header. The
   request names the operation (`purchase_high_value`), the target (`erp`) and the tool (`erp.create_po`), plus a
   payload. EACP checks the agent against the registry (the agent version is active, the tool is on its allowlist)
   and asks the policy decision point. The policy's `high-value` rule says `escalate`, so the action waits in
   `PENDING_APPROVAL` with an approval request that needs two approvers.
2. **Retry.** Agents retry: networks fail and processes restart. The same idempotency key with the same body
   returns the same action, so a retry never creates a second purchase order. The same key with a different body
   is refused with `409 idempotency_conflict`.
3. **Approve.** amy and ben each vote through `POST /v1/approvals/{id}/votes`. The first vote leaves the request
   `PENDING` and the second reaches the quorum: `GRANTED`. The grant is bound to the exact payload the policy saw
   (its digest) and can be used once. Separation of duties is enforced in PostgreSQL: only humans vote, the
   action's subject (sam) and the agent's owner cannot approve it, and nobody votes twice.
4. **Release and execute.** EACP asks the policy again at the release boundary. The answer is the same
   `escalate`, and the grant satisfies it, so the action becomes `AUTHORIZED`. The execution worker, the only
   process that holds the ERP credential, claims the action, records a dispatch intent before it calls anything,
   calls the Fake ERP once and records the result: `SUCCEEDED` with the ERP's order reference.
5. **Evidence.** An operator reads `GET /v1/actions/{id}/evidence`. It holds both policy decisions (at submission
   and at release), the approval with its votes and the consumed grant, every attempt, and the journal entries
   that trace each state change. The journal is a hash chain, and EACP verifies it while reading the evidence.

## What it demonstrates

- The agent never touches the ERP credential. The execution worker holds it (ADR-001).
- A human decision is bound to the exact payload and used once (ADR-005).
- Retrying is safe: one idempotency key gives one action (ADR-004).
- Every step leaves evidence that can be checked afterwards, not just a log line.
