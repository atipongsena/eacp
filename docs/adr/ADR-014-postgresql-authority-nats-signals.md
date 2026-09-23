# ADR-014: PostgreSQL as Execution Authority; NATS for Signals

- **Status:** Accepted — Rev 1.0 (Phase 10, 2026-09-24)
- **Date:** 2026-09-24
- **Phase 0 gate:** no (Slice B)
- **Related:** MASTER_PLAN §22, §42, §60, §62, §63, §84, §103 (inv. 1, 4, 8, 12); ADR-001, ADR-004 (principle 2, "Outbox"), ADR-005 §5a
- **Upstream (verified):** `github.com/nats-io/nats.go` v1.54.0 (`jetstream` package), `nats-server` v2.15.0 (tests embed it; compose runs the `nats:2.15.0-alpine` image). The APIs used are listed in `research/REFERENCES.md`.

## Context

Slice A keeps all execution state in PostgreSQL. Workers poll it, claim under a fenced lease, and commit a dispatch intent before any external call (ADR-004). The release transaction and every re-queue already write an `action.queued` row to `eacp.outbox_events`, but nothing publishes it: "outbox publishing remains open" (ADR-004).

MASTER_PLAN §60 brings NATS JetStream into Slice B for work-available hints and execution events. §84 (Phase 10) scopes it to four things: the outbox → NATS relay, inbox dedup, `action_id`-only work hints, and an event stream for dashboards. It also says correctness must not depend on NATS.

## Decision

### 1. NATS carries signals, never authority

- PostgreSQL is the only execution authority. A NATS message is a **hint**. No state is derived from it, and no decision depends on its arrival, order or absence.
- Assume every message can be duplicated, lost, delayed or reordered (§60).
- A worker never executes from a message. A hint only wakes the worker's claim loop, which claims and fences through PostgreSQL exactly as before (§22, ADR-004). Its polling stays on as the backstop, so the system is correct without NATS, just slower.
- NATS is not a readiness dependency. An unreachable server at startup is a warning, the client reconnects forever, and `/readyz` doesn't check it.

### 2. The transactional outbox

- The outbox is written in the transaction that changes the action (§62). That is already true for `action.queued`.
- Migration 00010 adds `action.transition`, written by a separate `AFTER INSERT OR UPDATE` trigger (`actions_outbox_events`). It sorts before `zz_audit`, so the audit append is still the last statement.
  - The trigger emits one row per state change, including the initial state.
  - Payload: `{"action_id", "from", "to", "at"}`. `from` is null for the initial state.
  - Reasons, payloads, digests, operation keys and external references are **excluded**. `state_reason` can hold free text from a human cancellation. A dashboard that needs detail reads the Action API, which applies authorization.
- Every row keeps the request's W3C `traceparent` (§42), when one was set.
- No secret ever enters the outbox (ADR-004).

### 3. The relay (controlplane-api)

With `EACP_NATS_URL` set, `controlplane-api` runs the relay. It makes a pass every 200 ms, and again at once after a full batch. Each pass does the following:

1. **Find work.** `eacp.outbox_pending(topics, limit)` returns the oldest unpublished `(tenant_id, id)` pairs across tenants. It is a reviewed SECURITY DEFINER hint, read-only, and returns ids only.
2. **Lock, per tenant.** In a tenant transaction bound to the `outbox` system actor, the relay locks the rows with `FOR UPDATE SKIP LOCKED`, so concurrent relays never publish the same row at once.
3. **Publish in `created_at` order,** each with JetStream's synchronous `PublishMsg`:
   - header `Nats-Msg-Id` = the outbox row id;
   - header `traceparent` = the row's traceparent;
   - body = the stored payload, as compact JSON. PostgreSQL renders jsonb with spaces, and the hint consumer accepts only the canonical form.
4. **Mark.** `published_at` is set **only after the stream's PubAck**, then the transaction commits. The first failed publish stops the batch; rows after it stay unpublished and keep their order.

Consequences for delivery:
- **Crash before commit.** A crash after a PubAck and before the commit republishes the row with the same `Nats-Msg-Id`. Within the stream's duplicate window (2 min) JetStream drops it (`PubAck.Duplicate`). After the window, the inbox (§5) drops it.
- **Delivery is at least once.** Never exactly once (§21).
- **What the lock touches.** The transaction locks only outbox rows, never an action row, so the lock order of ADR-005 §5a is untouched. `EACP_NATS_PUBLISH_TIMEOUT` (default 5s) bounds all the publishes of one tenant's batch together, so a slow or stalled NATS holds one pool connection, and the batch's outbox row locks, for at most that long.

**Messaging actors.** `outbox` and `inbox` are bound with `storage.SetSystem`. They are **messaging actors**:
- Only the outbox and inbox guards accept them, through `eacp.assert_messaging_actor`.
- `eacp.actor_context()` does not accept them. A transaction bound to one can't change an action, an approval, the registry or anything journaled.
- The existing system actors (`sweeper`, `worker`, `reconciler`) can't publish, prune or record an inbox message.

Database guards (migration 00010):
- **`published_at`.** Only the `outbox` actor may set it, once, from NULL. The trigger sets the value itself, and no other column may change.
- **`DELETE`.** Only the `outbox` actor may delete a row. The row must have been published at least an hour ago, or created at least 24 hours ago and never published.
- **Pruning.** It runs in `controlplane-api` whether or not NATS is configured, so a Slice A deployment's outbox stays bounded. An unpublished row older than a day is a lost hint, which §60 allows.
- **No journal entry.** Publishing and pruning aren't journaled. They change no execution state, and the rows carry no authority.

### 4. Subjects, streams and consumers

The relay owns the topology. It creates or updates it at startup and again after any publish error.

| Topic | Subject | Stream |
|---|---|---|
| `action.queued` | `eacp.work.<tenant_id>.action.queued` | `EACP_WORK` |
| `action.transition` | `eacp.events.<tenant_id>.action.transition` | `EACP_EVENTS` |

**`EACP_WORK`** (`eacp.work.>`) holds the work hints.
- Retention: work-queue, file storage, max age 1h, duplicate window 2 min, at most 1,000,000 messages (the oldest are discarded).
- It has one durable pull consumer, `execution-worker`, shared by all workers:
  - explicit acks;
  - `AckWait` 30s;
  - `MaxDeliver` 5.

  An old hint is worthless, because polling has picked the action up long before.

**`EACP_EVENTS`** (`eacp.events.>`) is the dashboard event stream.
- Retention: limits, file storage, max age 24h, at most 1 GiB, duplicate window 2 min.
- Dashboards read it with their own (ordered) consumers, filtered on `eacp.events.<tenant_id>.>`.
- The tenant sits in the subject so NATS permissions can scope a dashboard credential to one tenant. That scoping is the deployment's job, as RLS is the database's (§69).
- Events may arrive out of order or twice. `from`/`to` let a consumer spot that. The Action API stays authoritative.

The unknown-topic rule is **fail closed**:
- A topic the relay doesn't know is never published. It stays unpublished, and pruning removes it later.
- The relay only asks for the topics it knows, so an unknown one can't stall it.

### 5. Inbox dedup (§63)

`eacp.inbox_messages (tenant_id, consumer, message_id, processed_at)` is a tenant table under the standard RLS policy.
- A consumer records a message in a tenant transaction bound to the `inbox` system actor, keyed by `(tenant_id, consumer, Nats-Msg-Id)`.
- **Duplicate** (the row exists): the message is ACKed and not processed again.
- **Retention:** rows may be deleted, only by the `inbox` actor, an hour or more after processing. Consumers prune their own expired rows in small batches as they go (24h retention).

The worker's hint consumer (`execution-worker`) handles each message as follows:
1. **Validate the message.**
   - The subject must be `eacp.work.<uuid>.action.queued`.
   - `Nats-Msg-Id` must be a UUID.
   - The body must be exactly `{"action_id": <uuid>}`, strictly decoded.

   A malformed hint is terminated (`Term`), never retried.
2. **Record it in the inbox.**
3. **Wake the claim loop.**
4. **ACK.**

A database error NAKs with a delay, so the hint is redelivered, up to `MaxDeliver`. A consumer that is missing (for example after NATS lost its storage) is rebound until the relay has provisioned it again.

The hint names an action but grants nothing. The woken loop claims whatever PostgreSQL says is claimable for this worker, which may be a different action, or nothing.

### 6. Transport and credentials

- **Configuration.** `EACP_NATS_URL` (`nats://` or `tls://`, one URL). An optional `EACP_NATS_CA_FILE` adds a CA for `tls://`.
- **Plain `nats://`** is allowed only for a loopback host, or when `EACP_ENV` is development or test. Staging and production must use `tls://`.
- **Credentials** go in the URL's user info, like the database DSN. The password is redacted from logs and from the logged configuration.
- **Compose.** It runs NATS on an internal `bus` network that only `controlplane-api` and `execution-worker` join, with one user per role:
  - `relay` may publish on `eacp.work.>` and `eacp.events.>` and use the JetStream API.
  - `worker` may only read and ACK its own consumer. It can't publish a hint or an event, and it can't create consumers.

  The agent runtime has no route to NATS.
- **No enterprise credential ever touches NATS** (ADR-001). Nor does a connector secret.

## Consequences

- Work hints cut the time from release to claim from up to one poll interval to about one relay pass. Workers keep polling, so losing NATS only costs latency.
- A worker that is busy when it receives a hint still ACKs it. The hinted action then waits for that worker's next free slot, which claims at once, or for another worker's poll. The shared consumer trades that for one inbox row per hint instead of one per worker.
- Every action transition now writes an outbox row. The indexes are partial, and pruning keeps the table bounded.
- Dashboards can follow execution live without querying PostgreSQL. What they see is best effort.

## Unresolved assumptions and conservative defaults

| Assumption | Default chosen |
|---|---|
| Whether an event may carry the state reason | No. Reasons can hold human free text; dashboards read details through the authorized API. |
| Per-tenant NATS credentials for dashboards | Deployment configuration. Compose ships no dashboard user; the subject scheme makes per-tenant permissions possible. |
| The relay transaction spans the publish | Yes, bounded by the publish timeout, and it locks outbox rows only. |
| Hint targeting (claiming exactly the hinted action) | Not done. A hint wakes the normal claim, so the claim order stays PostgreSQL's (§57) and the scheduler (Phase 12) is unaffected. |
| Unpublished rows older than 24h | Pruned. That is a lost hint or event, which §60 allows; polling covers work. |
| Kill propagation, registry, health and reconciliation events (§60) | Not in Phase 10 (§84). They will reuse this relay with new topics. |
| The compose NATS container runs as the image's root user | Development only. It is on an internal network with per-role users, and production runs NATS under its own hardening. |
| Outbox and inbox rows of a tenant that stops receiving messages | The pruner handles the outbox for every tenant. An inbox keeps at most its last day of rows until the next message arrives for that tenant. |

## Verification

All of the following pass with `-race`.

- `internal/messaging`, against a JetStream server embedded in the test process and PostgreSQL:
  - Relay and dashboard stream:
    - `TestRelayPublishesEachRowOnceWithItsIDAndTraceparent`
    - `TestRepublishedRowIsDeduplicatedByTheStream`
    - `TestConcurrentRelaysPublishEachRowOnce`
    - `TestNATSOutageLeavesRowsUnpublishedUntilItRecovers`
    - `TestUnknownTopicsAreNeverPublished`
    - `TestDashboardEventsArrivePerTenant`
  - Hint consumer:
    - `TestHintWakesOnceAndADuplicateIsAckedWithoutWaking`
    - `TestMalformedHintsAreTerminatedWithoutWaking` (9 malformed shapes)
    - `TestHintConsumerBindsOnceTheTopologyExists`
  - End to end with a real worker:
    - `TestHintedWorkerExecutesLongBeforeItsPollInterval` (1-minute poll)
    - `TestWithoutNATSTheWorkerStillExecutes`
  - Raw SQL as `eacp_app`:
    - `TestTransitionsWriteDashboardEvents`
    - `TestOutboxRowsComeOnlyFromTheActionTriggers`
    - `TestOnlyTheOutboxActorPublishesARowOnce`
    - `TestOutboxPruningIsGuarded`
    - `TestInboxIsGuardedAndTenantIsolated`
- `internal/worker`:
  - `TestWakeClaimsBeforeThePollInterval`.
  - `TestAnotherTenantSeesAndChangesNothingAfterAFullFlow`, which now includes an inbox record.
  - Every existing poll test, all of which run without NATS.
- `internal/storage` RLS catalog: `owner_scan` on `outbox_events`, `eacp.outbox_pending` and `eacp.outbox_prunable` are reviewed.
- `test/security`:
  - The agent can't reach NATS.
  - The worker credential can't publish on `eacp.work.>`, `eacp.events.>` or the stream API.
  - Positive control: the relay credential can publish.
  - Both services are wired, and no NATS password reaches their logs.
- `test/demo` step 12:
  - Before the outage, the relay has published every outbox row.
  - With NATS stopped, a purchase executes once by polling and its rows wait.
  - After a restart, the rows drain.
