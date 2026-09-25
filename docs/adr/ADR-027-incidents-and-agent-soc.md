# ADR-027: Incidents and the Agent SOC read model

Status: Accepted (Rev 1.0, 2026-09-25). Scope: Phase 22a (MASTER_PLAN §54–§56 and §94).
Related: ADR-015 (dependency graph and blast radius), ADR-016 (kill switch), ADR-018 (releases), ADR-022 (circuits),
ADR-023 (MCP registry), ADR-025 (FinOps), ADR-004 (unknown outcomes).

## Context

MASTER_PLAN §94 asks for an Agent SOC: one place that shows what is wrong now and what it touches. The signals already
exist in PostgreSQL:
- a tool the MCP scanner quarantined because its certified definition changed;
- an active kill scope;
- a connector circuit a worker's breaker opened;
- an action in `NEEDS_HUMAN_RESOLUTION`;
- a canary the release system actor rolled back;
- an unacknowledged FinOps overspend or anomaly.

Nothing correlates these signals, records who is handling them, or keeps a timeline. Phase 22 is split:
- **22a** (this ADR): the incident domain, the SOC summary API and eacpctl;
- **22b**: the operator web UI on top of them.

## Decision

### 1. Observation, never authority

An incident never blocks, contains, releases or decides anything. Responses stay with the existing APIs (tool
quarantine, kill, fleet pause, release rollback), called by the operator with their own key under the existing rules.
There are no automated playbooks. This is the same rule as ADR-015 and ADR-025. Nothing is added to action, kill,
circuit or scan transactions, and the lock order does not change.

### 2. Model (migration 00022)

`eacp.incidents` stores the following, under the RLS convention:
- `kind`;
- `source_key`, the signal occurrence. It is unique per tenant and kind, and NULL only for `manual`;
- `severity`, one of `low`, `medium`, `high`, `critical`;
- `state`, which moves `OPEN` → `ACKNOWLEDGED` → `RESOLVED`;
- `title`;
- an optional subject;
- `detail`, at most 16 KiB of ids, states, counts, epochs and database text: never an action payload, a
  `state_reason`, a kill reason or a secret;
- `affected`, at most 64 KiB: the blast-radius snapshot at opening;
- the lifecycle columns.

`eacp.incident_events` is the insert-only timeline, numbered per incident under the incident's row lock. Every event
appends one audit event in the same transaction, last.

### 3. Lifecycle (guards)

- **Opening.**
  - Only the `incident` system actor, with no principal, opens automatic incidents.
  - Only an `operator` or `admin` opens a manual incident. It needs a reason (`detail` carries nothing else) and an
    optional subject that exists in the tenant.
- **Moves.** An `operator` or `admin` changes one thing at a time:
  - acknowledges (`OPEN` → `ACKNOWLEDGED`, with a reason);
  - assigns (to an enabled operator or admin, or to nobody);
  - resolves (`ACKNOWLEDGED` → `RESOLVED`, with a code of `contained`, `false_positive`, `accepted_risk` or
    `duplicate`, and a reason).
- **Two-person resolution.** A `critical` incident is resolved by someone other than its acknowledger. Severity is
  immutable, so this rule cannot be bypassed.
- **Terminal state.** `RESOLVED` is terminal. A later occurrence of the signal is a new incident.
- **Timeline.** Lifecycle events come only from the incident's own trigger. Notes and links (to an action, kill state,
  fleet operation, release, change set, tool, connector, agent, agent version or FinOps alert in the tenant) come from
  an operator or admin, in any state.
- **Privileges.** `eacp_app` may update only the lifecycle columns and may delete nothing.

### 4. The evaluator

`eacp.incident_evaluate()` runs per tenant as `storage.SetSystem("incident")`. controlplane-api runs it every
`EACP_INCIDENT_INTERVAL` (default 15s, at least 5s), for the tenants that `eacp.incident_tenants()` names. That
function is a SECURITY DEFINER hint that returns ids only. The evaluator reads the signal tables without locks and
writes only incidents, with `ON CONFLICT DO NOTHING`. Only current signals open an incident:

| kind | signal | source_key | severity |
|---|---|---|---|
| `mcp_drift` | tool quarantined by the scanner | `tool:<id>:<quarantined_at µs>` | high, critical if affected |
| `kill` | active kill state | `<scope>:<target>:<epoch>` | critical for `tenant`; high, critical if affected |
| `circuit_open` | circuit opened by a worker, still open | `connector:<id>:<changed_at µs>` | medium |
| `unknown_outcome` | action in `NEEDS_HUMAN_RESOLUTION` | `action:<id>` | high |
| `canary_rollback` | automatic rollback within 7 days | `release:<id>` | critical on production, else high |
| `finops` | unacknowledged `soft_limit_exceeded` or `spend_anomaly` | `finops_alert:<id>` | medium |

"Affected" means a confirmed affected version that is `ACTIVE` on a production agent. Some changes are not incidents,
because the operator has already acted and the change is journaled:
- an operator-disabled circuit;
- an operator-made quarantine.

### 5. One blast-radius rule

The ADR-015 recursive CTE now lives in `eacp.blast_radius_versions(nodes)`. Reachability from a set of nodes is the
union of each node's, so one recursive query serves a whole scope. `registry.BlastRadius` (with one node) and
`eacp.incident_affected(nodes)` both call it, so the rule exists once.

Each kind of scope maps to nodes:
- a tool or an agent version: itself;
- an agent: its versions;
- a team: the versions of the agents the group owns;
- an action: its agent version;
- a connector: its tools, plus `mcp:<id>` for an MCP server;
- a tenant: no nodes, but its kill is always critical.

The snapshot is bounded, so a large connector or team never fails the 64 KiB check and never stops the evaluator:
- the first 100 nodes, and `node_count`;
- up to 100 confirmed versions (agent, version, environment, state);
- `confirmed_count` and `possible_count`;
- `production_active`.

Unknown or stale evidence still widens possible impact.

### 6. The SOC summary

`GET /v1/soc/summary` reads one tenant read-only snapshot:
- **agents:** registered, production, high risk, owner disabled, versions by state;
- **security:** open incidents by severity, unacknowledged, quarantined tools, active kills, open and disabled
  circuits, pending approvals;
- **execution:** queued, running, retry wait, unknown outcome, needs human;
- **FinOps:** open alerts, and today's spend by unit, where spend is what the FinOps dashboard counts (committed and
  held tool cost plus effective LLM cost since UTC midnight).

### 7. API and CLI

| route | roles |
|---|---|
| `GET /v1/incidents?state=&severity=&kind=&limit=` (≤ 500), `GET /v1/incidents/{id}` | operator, auditor, admin |
| `POST /v1/incidents`, `/{id}/acknowledge`, `/assign`, `/notes`, `/links`, `/resolve` | operator, admin |
| `GET /v1/soc/summary` | operator, auditor, admin |

The eacpctl commands are `incident list|show|open|ack|assign|note|link|resolve` and `soc summary`.

## Consequences

- Operators get one correlated, journaled view of the signals and of who is handling them. No new authority is
  introduced.
- Detection latency is the evaluator interval. Hot paths pay nothing.
- A no-op move is rejected, including assigning the current assignee. The API answers 409 and writes no timeline
  event.
- A signal that persists after resolution does not reopen its incident. Only a new occurrence opens a new one (a new
  quarantine time, kill epoch or circuit change).

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Detection latency | The evaluator interval (default 15s); never in hot paths. |
| Resolving while the signal persists | Allowed with a code and reason; the occurrence stays resolved. |
| Critical resolution | Two people: resolver ≠ acknowledger. |
| Kill reason text | Not copied into `detail` (reason code and epoch only). |
| Notes | Free text by operators, journaled; operators must not paste secrets (as with every reason field). |
