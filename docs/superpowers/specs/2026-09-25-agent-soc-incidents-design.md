# Phase 22a — Incidents and the Agent SOC read model (design)

Date: 2026-09-25 · Status: approved by the owner (Section 1 in chat; the rest delegated: "start dev until finish phase")
Scope: MASTER_PLAN §54–§56, §94 (Phase 22). Phase 22 is split: **22a** (this spec) is the incident domain, the
SOC summary API and eacpctl; **22b** is the operator web UI on top of them (a later spec).

## 1. Intent

Operators need one place that says what is wrong right now and what it touches: an MCP tool that changed
under a certified contract, a kill switch that is holding work, a connector whose breaker opened, an action
whose outcome nobody knows, a canary that rolled back, spend over its soft limit. Every one of these signals
already exists in PostgreSQL; nothing correlates them, records who is handling them or keeps a timeline.

Success: the Slice C demo's MCP drift and kill (§100) show up as incidents with the affected agents, an
operator acknowledges and links the response, and a second operator resolves the critical one. The SOC
summary shows the §55 counters from one read-only snapshot.

## 2. Principles (non-negotiable)

1. **Observation, never authority.** An incident never blocks, contains, releases or decides anything. It
   recommends responses as pointers to the existing APIs (tool quarantine, kill, fleet pause, release
   rollback); the operator calls them with their own key under the existing rules. Same rule as ADR-015
   (dependency graph) and ADR-025 (FinOps). No automated playbooks.
2. **PostgreSQL decides.** Tables, guards, the evaluator, severity and the affected snapshot are in
   PostgreSQL. Go never sends a severity, affected set, title or detail for an automatic incident.
3. **Journaled.** Every incident event (opened, acknowledged, assigned, note, linked, resolved) is written
   to the hash-chained audit journal in the same transaction, as its last statement.
4. **No secrets, no payloads.** `detail` holds ids, states, counts, epochs and DB-generated text only —
   never an action payload, an action's `state_reason`, a connector response or a secret.
5. **Hot paths untouched.** Nothing is added to action, kill, circuit or scan transactions. The evaluator
   reads source tables without locks and writes only incident rows; lock order (AGENTS.md) is unchanged.

## 3. Data model (migration 00022)

### 3.1 `eacp.incidents`

| column | notes |
|---|---|
| `tenant_id`, `id` | RLS convention |
| `kind` | `mcp_drift`, `kill`, `circuit_open`, `unknown_outcome`, `canary_rollback`, `finops`, `manual` |
| `source_key` | the signal occurrence; NULL only for `manual`; UNIQUE (tenant, kind, source_key) |
| `severity` | `low`, `medium`, `high`, `critical` |
| `state` | `OPEN` → `ACKNOWLEDGED` → `RESOLVED` |
| `title` | ≤ 200 chars |
| `subject_type`, `subject_id` | `tool`, `kill_state`, `connector`, `action`, `release`, `finops_alert`, or NULL (manual) |
| `detail` | jsonb object ≤ 16 KiB |
| `affected` | jsonb object ≤ 64 KiB: blast-radius snapshot at opening |
| `opened_by` (NULL = the `incident` system actor), `opened_at` | |
| `acknowledged_by`, `acknowledged_at`, `ack_reason` | |
| `assignee_id` | an enabled principal holding `operator` or `admin` |
| `resolved_by`, `resolved_at`, `resolution`, `resolution_reason` | `resolution` ∈ `contained`, `false_positive`, `accepted_risk`, `duplicate` |

### 3.2 `eacp.incident_events` (insert-only timeline)

`tenant_id, id, incident_id, seq (per incident, from 1), kind, actor_id (NULL = system), at, note,
link_kind, link_id`. Kinds: `opened`, `acknowledged`, `assigned`, `resolved` (written only by the
incidents trigger, `pg_trigger_depth() > 1`), `note` and `linked` (inserted directly by an operator or
admin). A note is 1–4096 characters. A link names `action`, `kill_state`, `fleet_operation`, `release`,
`change_set`, `tool`, `connector`, `agent`, `agent_version` or `finops_alert` and must exist in the tenant. Notes
and links are allowed in every state (post-incident review). Each row appends one audit event.

### 3.3 Guards

- **Insert.** An automatic incident (`kind <> 'manual'`) only from `eacp.incident_evaluate()` as the
  `incident` system actor with no principal; it starts `OPEN`. A manual incident only by a principal
  holding `operator` or `admin`, with a title, a severity, an optional subject and `detail.reason`;
  `affected` is `{}`. The trigger fills `opened_by`, `opened_at`, `state`.
- **Update** (principal holding `operator` or `admin`; only these moves; every other column is immutable):
  - acknowledge: `OPEN` → `ACKNOWLEDGED`, reason required;
  - assign: `assignee_id` changes while not `RESOLVED`; the assignee is an enabled `operator`/`admin`;
  - resolve: `ACKNOWLEDGED` → `RESOLVED` with a resolution code and a reason. **A `critical` incident is
    resolved by a principal other than the one who acknowledged it** (two-person, §57).
- `RESOLVED` is terminal. A later occurrence of the same signal is a new incident.
- The `eacp_app` role may update only the lifecycle columns (column grants) and never delete.

### 3.4 The evaluator — `eacp.incident_evaluate()`

Runs as `storage.SetSystem("incident")` per tenant (controlplane-api, `EACP_INCIDENT_INTERVAL`, default
15s, at least 5s). Idempotent: `INSERT … ON CONFLICT (tenant_id, kind, source_key) DO NOTHING`.
It returns the number of incidents opened. Only **current** signals open an incident:

| kind | signal (current) | source_key | severity |
|---|---|---|---|
| `mcp_drift` | a tool quarantined by the scanner (`quarantined_at` set, `quarantine_changed_by_worker` set) | `tool:<id>:<quarantined_at µs>` | `high`; `critical` if a confirmed affected version is ACTIVE on a production agent |
| `kill` | an active kill state | `<scope>:<target>:<epoch>` | `critical` for `tenant`; `high` otherwise, `critical` if a confirmed affected version is ACTIVE on a production agent |
| `circuit_open` | a circuit opened by a worker (`open_until > now()`, `changed_by_worker` set) | `connector:<id>:<changed_at µs>` | `medium` |
| `unknown_outcome` | an action in `NEEDS_HUMAN_RESOLUTION` | `action:<id>` | `high` |
| `canary_rollback` | an automatic rollback (`ROLLED_BACK`, `changed_by IS NULL`) in the last 7 days | `release:<id>` | `high`; `critical` on a production agent |
| `finops` | an unacknowledged `soft_limit_exceeded` or `spend_anomaly` alert | `finops_alert:<id>` | `medium` |

Operator-disabled circuits and operator-made quarantines are not incidents: the operator already acted and
the change is journaled.

### 3.5 The affected snapshot

`eacp.blast_radius_versions(p_node text)` returns `(version_id, confirmed, possible)` for every agent version
— the ADR-015 recursive CTE, moved from Go into one SQL function that `registry.BlastRadius` now calls too,
so the rule exists once. `eacp.incident_affected(p_nodes text[])` builds the snapshot:
`{"nodes": [...], "confirmed": [{agent_id, agent, version_id, version, environment, state}] (≤ 200),
"confirmed_count", "possible_count", "production_active": n}`. Nodes per kind: `tool:<id>`; a kill's
`agent_version:<id>`, `tool:<id>`, the agent's versions, a connector's `mcp:<id>` plus its tools (tenant,
team and action kills: no nodes); a circuit's connector as above; an action's version; a release's
candidate version. Possible impact keeps ADR-015's widening: unknown evidence is never proof of no impact.

### 3.6 Cross-tenant hint

`eacp.incident_tenants()` (SECURITY DEFINER, reviewed in the RLS catalog) returns tenant ids with a current
signal (a quarantined tool, an active kill, an open circuit, an action needing a human, a recent rollback,
an open FinOps alert). Ids only.

## 4. Go — `internal/incident`

- `Service`: `List(filter: state, severity, kind, limit ≤ 500)`, `Get(id)` (incident + timeline), `Open`
  (manual), `Acknowledge`, `Assign`, `Note`, `Link`, `Resolve`; `Evaluate(tenant)`, `EvaluateAll`, `Run`.
- `Summary(actor)`: the SOC read model, one `InTenantReadTx`:
  - agents: registered, production, high risk (`high`/`critical`), versions by state (ACTIVE, SUSPENDED,
    QUARANTINED), agents whose owner principal is disabled;
  - security: open incidents by severity, unacknowledged, quarantined tools, active kills, open and
    disabled circuits, pending approvals;
  - execution: queued, running (`LEASED`, `EXECUTING`), retry wait, unknown outcome (`UNKNOWN_OUTCOME`,
    `RECONCILING`), needs human;
  - finops: open alerts, spend today by unit (`tool_committed + llm_effective` of `eacp.finops_agent_spend`
    since UTC midnight).
- Errors map like `internal/kill` (42501 → forbidden, 23514/22P02 → invalid, 23503 → not found, 55000 →
  conflict).

## 5. API and eacpctl

| route | roles |
|---|---|
| `GET /v1/incidents?state=&severity=&kind=&limit=` | operator, auditor, admin |
| `GET /v1/incidents/{id}` | operator, auditor, admin |
| `POST /v1/incidents` (manual) | operator, admin |
| `POST /v1/incidents/{id}/acknowledge` · `/assign` · `/notes` · `/links` · `/resolve` | operator, admin |
| `GET /v1/soc/summary` | operator, auditor, admin |

`eacpctl incident list|show|open|ack|assign|note|link|resolve` and `eacpctl soc summary`.

## 6. Testing

- Raw SQL as `eacp_app` for every guard: only the system actor opens automatic incidents; manual needs an
  operator; the lifecycle moves; immutability; critical two-person resolve; timeline events only from the
  trigger; notes and links; audit per event; tenant isolation.
- Evaluator: each signal kind opens exactly one incident, re-evaluation opens none, a resolved occurrence
  stays resolved, a new occurrence opens a new incident, severity escalation on production, the affected
  snapshot, and `registry.BlastRadius` unchanged (its tests stay green on the SQL function).
- Service, API (role gates, 403/409 mapping), eacpctl command tests, the SOC summary counters.
- RLS catalog (2 tables, 1 SECURITY DEFINER function), worker isolation fixture, INVARIANTS §17.
- Demo C: after the kill (C5), the MCP drift and kill incidents appear with the affected agent; otto
  acknowledges and links the kill; otto cannot resolve the critical one; opal resolves it.
- Full `go vet ./... && go test -race ./...` with the test database.

## 7. Documents

ADR-027 (Incidents and the Agent SOC read model), ADR index, MASTER_PLAN §94 status (22a delivered, 22b
UI next), INVARIANTS, AGENTS.md status and rule, DEMO.md.

## 8. Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Detection latency | Evaluator interval (default 15s); never in hot paths. |
| Resolving while the signal persists | Allowed with a code and reason; the occurrence stays resolved. A new occurrence (new quarantine time, kill epoch, circuit change) opens a new incident. |
| Critical resolution | Two people: resolver ≠ acknowledger. |
| Kill reason text | Not copied into `detail` (reason code and epoch only). |
| Notes | Free text by operators, journaled; operators must not paste secrets (same as every reason field). |
