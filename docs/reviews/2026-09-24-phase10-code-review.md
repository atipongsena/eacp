# Phase 10 NATS JetStream Review

Date: 2026-09-24
Scope: Slice B Phase 10 (MASTER_PLAN §84, with §60, §62 and §63; ADR-014 Rev 1.0; §103 invariants 4 and 8).
Review: a self-review against ADR-014 and the invariants below, plus mutation checks of the new tests.

## Invariants stated before the code

1. **PostgreSQL is the only authority.** A NATS message never causes a claim, a dispatch or a decision. It only ends the worker's idle wait. The claim is the same fenced PostgreSQL claim as in Slice A.
2. **Correctness without NATS.** Polling stays on. A NATS outage delays hints and events. It never loses execution state, and it never blocks startup or readiness.
3. **At least once, never marked early.** An outbox row is marked published only after the stream's PubAck. Repeats are dropped by `Nats-Msg-Id` (the stream's duplicate window) and then by the inbox.
4. **Minimal content.** A hint carries only `action_id`. An event carries ids, states and a time: never a reason, payload, digest, operation key, external reference or secret. Outbox rows come only from the action triggers.
5. **Tenant isolation.**
   - The inbox is a tenant table under RLS.
   - The new cross-tenant paths are two reviewed SECURITY DEFINER hints that return `(tenant_id, id)` only.
   - Every write runs in its own tenant transaction.
   - The tenant is in the subject, so NATS permissions can scope consumers.
6. **No new power.** The messaging actors (`outbox`, `inbox`) can't change an action. The worker's NATS credential can't publish.

## Implemented

- **ADR-014** (new; Accepted Rev 1.0). The ADR-004 outbox note now points to it.
- **Migration 00010.**
  - `action.transition` outbox rows come from a separate trigger (`actions_outbox_events`, which sorts before `zz_audit`).
  - Outbox rows may only come from triggers (`pg_trigger_depth`).
  - Publish and prune guards, and the retention rule `eacp.outbox_expired`.
  - `eacp.inbox_messages` with RLS and its guard.
  - `eacp.assert_messaging_actor`, the `owner_scan` policy on `outbox_events`, and `eacp.outbox_pending` / `eacp.outbox_prunable`.
- **`internal/messaging`:**
  - `Topology.Provision`: the streams `EACP_WORK` and `EACP_EVENTS`, and the durable consumer `execution-worker`.
  - `Relay`, `PruneOutbox` / `RunPruner`, `Inbox`, and `Hints` (the strict parser, inbox dedup and wake-up).
  - `Connect`: retries forever, has no reconnect buffer, and accepts an optional CA.
  - `natstest` embeds a JetStream server, so the tests never skip.
- **Worker.** `worker.Options.Wake` ends the idle wait early. Nothing else in the worker changed.
- **Binaries.**
  - `controlplane-api` always prunes the outbox. With `EACP_NATS_URL` set, it also relays (every 200 ms).
  - `execution-worker` runs the hint consumer and passes its wake-up channel to the worker.
- **Config.**
  - `EACP_NATS_URL`: one server. Plain `nats://` is allowed only for loopback or in development and test.
  - `EACP_NATS_CA_FILE` (needs `tls://`) and `EACP_NATS_PUBLISH_TIMEOUT`.
  - The URL's password is redacted from the logged configuration and from every log line.
- **Compose.**
  - `nats:2.15.0-alpine` on an internal `bus` network (API and worker only).
  - One user per role: `relay` owns the topology and publishes. `worker` may only look up its consumer, pull from it and ACK.
- **Demo.** New step 12: NATS outage and recovery. The later steps are renumbered.

## Review findings and fixes

1. **System actors would have widened action privileges.** At first, `outbox`/`inbox` were going to be added to `eacp.actor_context()`. The action guard allows several moves (T2–T5, T10–T13, T15) to any non-owner system actor, so a relay transaction could have authorized or released an action.
   - **Fix.** `actor_context()` is unchanged and still rejects both. Only `eacp.assert_messaging_actor` accepts them, and only the outbox and inbox guards call it.
   - **Tests.** Every combination of actor and guard is covered in `schema_test.go`.
2. **Any bound actor could insert outbox rows.** Before 00010, an agent with a SQL session could insert arbitrary hints or events. With a dashboard stream, that becomes forged events.
   - **Fix.** The insert guard now requires trigger depth ≥ 2 (`TestOutboxRowsComeOnlyFromTheActionTriggers`).
3. **An event could have leaked human text.** `state_reason` holds free-text cancellation reasons, so events carry no reason (`TestTransitionsWriteDashboardEvents` cancels with free text and checks the event).
4. **The reconnect buffer would have undermined "marked only after PubAck".** With nats.go's default buffer, a publish made while disconnected is flushed after reconnecting, after the relay has already given up on it.
   - That is harmless, since it is a duplicate and gets deduped, but confusing. `ReconnectBufSize(-1)` makes such publishes fail at once.
5. **jsonb rendering.** PostgreSQL renders `{"action_id": "…"}` with a space, and the consumer accepts only the canonical compact form. The relay compacts the body.
6. **Unbounded transaction time.** Per-publish timeouts would let a slow but live NATS hold a tenant's relay transaction for up to a batch of timeouts. One timeout now bounds the whole batch.
7. **Vacuous isolation sweep.** `TestAnotherTenantSeesAndChangesNothingAfterAFullFlow` requires every table to have tenant A rows. Its flow now records an inbox message, so the new table is covered by the cross-tenant read/update/delete sweep.
8. **Outbox growth.** Every transition now writes a row, and a Slice A deployment has no relay. The pruner runs regardless of NATS, deleting published rows after 1h and unpublished rows after 24h.

## Mutation checks

Each of these was introduced on purpose, caught by the named test, and reverted.

| Mutation | Caught by |
|---|---|
| Mark every locked row published, even after a failed publish | `TestNATSOutageLeavesRowsUnpublishedUntilItRecovers` |
| Wake the worker on duplicate hints | `TestHintWakesOnceAndADuplicateIsAckedWithoutWaking` |
| Drop `FOR UPDATE SKIP LOCKED` from the relay | `TestConcurrentRelaysPublishEachRowOnce` (the publish guard also rejects the second mark with 55000) |

## Accepted residual risks

- The compose NATS runs as the image's root user, with plaintext development passwords in `deployments/docker/nats/nats.conf`. This is development only: the network is internal and the users are per role. Production uses `tls://`, its own secrets and its own hardening.
- A dashboard credential is not shipped. Per-tenant scoping (`eacp.events.<tenant>.>`) is deployment configuration, documented in ADR-014 §4.
- A worker that is busy when it gets a hint still ACKs it. The hinted action waits for that worker's next free slot, or for another worker's poll. The latency cost is bounded by the poll interval.
- An inbox keeps at most its last day of rows for a tenant that stops receiving messages. It is pruned when that tenant's next message arrives.
- Kill propagation, registry, health and reconciliation events (§60) are outside §84. They will be new topics on this relay.

## Verification

- `go vet ./...` and `go test -race ./...` with `EACP_TEST_ADMIN_DSN`: all packages pass, including 16 new `internal/messaging` tests against embedded JetStream.
- `EACP_COMPOSE_TEST=1 go test ./test/security/`: 16 tests pass, 4 of them new (NATS reachability, credential permissions with a positive control, wiring and log redaction).
- `scripts/demo.sh`: passes in 76 s. In step 12, 65 rows are published before the outage; the purchase executes once by polling while 7 rows wait; after the restart, the rows drain.
