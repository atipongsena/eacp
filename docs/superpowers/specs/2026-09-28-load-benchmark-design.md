# The Load Benchmark (design)

Date: 2026-09-28 · Status: approved by the owner in chat, sections 1–3. Choices: black-box on the compose stack
(approach A: a `cmd/eacp-bench` binary driven by `scripts/bench.sh`); N registered agents with an open-loop arrival
rate; the action path under both PDPs (local and the Microsoft AGT sidecar) plus an LLM gateway scenario.
Scope: MASTER_PLAN §104 (Load Benchmark) and §105 (Never Fake Benchmarks). No ADR: the benchmark decides nothing and
changes no service; the MASTER_PLAN §104 status records it.

## 1. Intent

EACP has no measured numbers. The only benchmark is `BenchmarkSchedulerFairness`, which exercises one query. The
portfolio needs real, reproducible figures for what one development machine can sustain, with the control plane's
own overhead kept separate from governance, the external API and the LLM (§105).

Success:
- one command, `scripts/bench.sh`, reproduces every figure on a fresh, isolated stack;
- each §104 measure is reported as P50/P95/P99 (and max) at 100, 1,000, 5,000 and 10,000 agents, under each PDP;
- the maximum sustainable rate is reported for each PDP and N;
- `docs/BENCHMARKS.md` holds the first real run, generated from committed raw results, never typed by hand;
- no number in the README or `docs/BENCHMARKS.md` exists without a raw result behind it.

## 2. Out of scope

- Scaling across tenants (a `--tenants` flag). Section 5 explains why it is the likely next step.
- Kubernetes and multi-replica runs.
- Approval (escalation) paths, since they wait on humans.
- Reconciliation and fault injection.
- A2A, MCP scans and streaming LLM calls.
- Real LLM providers and a real ERP. Their latency is not EACP's to claim.
- A Prometheus `/metrics` endpoint. No service code changes.

## 3. Design

### 3.1 Components

- **`scripts/bench.sh`** starts a fresh compose project `eacp-bench` with its own volumes, as `scripts/demo.sh` does,
  with `docker-compose.yml` plus `deployments/bench/compose.bench.yml`. It runs `eacp-bench` against it and removes the
  project and its volumes afterwards; `KEEP=1` keeps it. A development stack (project `eacp`) and the demo stack are
  never touched. It runs once per PDP: `local`, then `microsoft-agt`, each on a new stack. `--quick` passes through.
- **`deployments/bench/compose.bench.yml`** is bench-only. Every published port binds 127.0.0.1:
  - API 28080, gateway 28083, PostgreSQL 55434;
  - NATS monitoring 28222, published for this project only;
  - `EACP_GOVERNANCE_PROVIDER` on the API is set from `BENCH_PDP`, and the AGT settings stay as in compose;
  - `EACP_WORKER_CONCURRENCY=32`;
  - `EACP_ACTION_MAX_QUEUED_PER_TENANT`, `EACP_ACTION_MAX_QUEUED_GLOBAL` and `EACP_ACTION_MAX_PENDING_PER_TENANT` are
    100000, so the run measures the system rather than the admission limit;
  - PostgreSQL runs with `track_functions=pl`.

  The report prints every override next to its default.
- **`cmd/eacp-bench`** is one process per PDP. It sets up the tenant, loads the agents, runs the load and collectors,
  and writes the raw results. `eacp-bench report <json>...` renders markdown. Agent keys live in this process's memory
  only: they are never logged and never written to the results.
- **`internal/bench`** holds the logic that can be tested without a stack:
  - the open-loop scheduler and its clock;
  - percentiles;
  - the per-stage computation from transitions;
  - the saturation rule;
  - the results schema and the markdown report.

### 3.2 Setup (not measured; its duration is reported)

- **Tenant.** Tenant `00000000-0000-4000-8000-0000000000ac` is the tenant the dev LLM secrets manifest names, so the
  gateway's `fakellm` credential applies without a bench-specific manifest; the plan verifies this. It is created
  with `eacpctl tenant create` (admins alice and bob), as in `test/demo`. People are erin (registry editor), rita and
  ravi (registry approvers). Every grant and key is approved by a second person.
- **Registry.**
  - Connector `erp` on `http://fakeerp:8090` uses the demo's ERP `secret_ref`.
  - Tool `create_po` uses the demo's native-idempotency AUTHORITATIVE contract, with cost fields (`cost_unit` THB,
    `cost_amount_field` `amount`, `cost_unit_field` `currency`), so every action reserves budget.
  - An allow policy is `{"match": {"target": "erp"}, "verdict": "allow"}`, activated by bob.
  - LLM model `sonnet` (anthropic, fakellm) has a rate-card price, as in the LLM demo.
- **Agents.** Agents are added cumulatively up to each N (100 → 1,000 → 5,000 → 10,000), with 16 concurrent setup
  workers. Each agent gets:
  - agent → version → allowlist (`erp.create_po`, model `sonnet`), approved by rita → `ACTIVE` by ravi;
  - an agent key (created by erin, approved by rita);
  - a THB budget account and a USD budget account, each with a limit raise that alice proposes and bob approves.
    The limits are large enough that no benchmark action is denied for budget. The plan verifies that an agent may
    hold one account per unit; if it may not, the LLM scenario runs unreserved and the report says so. A budget
    denial is counted as an error.

### 3.3 Load

- **Open loop.** Each step sends at a fixed rate. Request i has an intended send time t0 + i/rate, and its latency is
  measured from that intended time, not from when it was actually sent (this corrects for coordinated omission). A
  slow server therefore shows accumulated latency instead of a slower sender. The sender does not wait for responses:
  in-flight requests are unbounded up to a safety cap of 20,000, and reaching the cap marks the step `incomplete`.
- **Request.** Each request is `POST /v1/actions` from a uniformly random agent among the current N, with a unique
  `Idempotency-Key` and payload `{"amount": 1, "currency": "THB"}`, plus `delay_ms` when `--erp-delay-ms` is set.
- **Idempotency replays.** Five percent of requests replay an earlier request's key and payload from the same agent.
  Their latency is the idempotency latency. After each step, SQL checks that every key has exactly one action and
  that every succeeded action has exactly one ERP operation.
- **Steps.** Action steps are 25, 50, 100, 200, 400 and 800 per second, each with a 10 s warm-up (excluded) and a
  60 s measurement. After each step the tool waits for the backlog to drain: every measured action must be terminal,
  within 5 minutes. A step that does not drain is `incomplete`. Climbing stops at the first saturated or incomplete
  step.
- **Saturation.** A step is saturated if any of these holds:
  - completed throughput is below 95% of the offered rate;
  - errors plus 429s exceed 1% of requests;
  - end-to-end P99 exceeds 5 s.

  The maximum sustainable rate is the highest step that is neither saturated nor incomplete.
- **LLM scenario.** After N = 10,000, the tool sends `POST /v1/messages` (non-streaming, model `sonnet`) through the
  gateway with random agents' keys, at 25, 50, 100 and 200 per second, using the same step rules.
- **`--quick`.** N = 100, action steps 25 and 50, and LLM step 25, each with a 5 s warm-up and a 20 s measurement. It
  exercises the harness end to end in a few minutes.

### 3.4 Measures

Every stage uses the action's journal transitions. `eacp.audit_events.recorded_at` is `clock_timestamp()` taken under
the chain head lock, and each payload carries `from` and `to` for `action.received` and `action.transition`. Stages
also use `eacp.action_attempts.dispatched_at` and `completed_at`. The collector reads these after the drain, through
the owner DSN of the bench PostgreSQL on 127.0.0.1, with read-only queries.

| §104 measure | Definition | Source |
|---|---|---|
| Admission latency | intended send → the `POST /v1/actions` response (includes the synchronous governance call) | client |
| Governance latency | `action.received` → the first transition out of `RECEIVED` (the PDP call and its surrounding transaction), per PDP | journal |
| Queue wait | → `QUEUED` to → `LEASED` (first lease) | journal |
| Lease latency | → `LEASED` to the first attempt's `dispatched_at` (claim to dispatch intent) | journal + attempts |
| External API | first attempt `dispatched_at` → `completed_at` | attempts |
| Execution overhead | end-to-end − governance − external API (the control plane's own time, §105) | computed |
| End-to-end | `action.received` → the terminal transition | journal |
| Idempotency latency | a replay's intended send → its response | client |
| Budget reservation | mean time per call of `eacp.budget_reserve` from the `pg_stat_user_functions` delta over the step, labelled "mean, not a percentile" | pg_stat |
| Admission / scheduling throughput | accepted submissions per second; actions that reached a terminal state per second over the step | client, journal |

Two rules apply to the stage figures:
- An action that lacks a stage is left out of that stage's figures and counted per stage. That covers retries (only
  the first attempt counts), `DENIED`, `UNKNOWN_OUTCOME` and a missing attempt.
- Terminal states are tallied per step.

The collector samples every 2 s during the measurement:
- **CPU and RAM:** mean and max CPU% and max memory per container (api, worker, agt-pdp, postgres, nats, fakeerp,
  llm-gateway, fakellm) from `docker stats --no-stream --format json`. The same figures for the `eacp-bench` process
  itself show whether the load generator is the bottleneck.
- **DB:** commits per second and cache hit ratio (`pg_stat_database` deltas), active connections and sessions waiting
  on a lock (`pg_stat_activity`), and database size.
- **NATS:** messages and bytes per second, in and out (`/varz` deltas), and JetStream totals (`/jsz`).

A source that cannot be read is reported as `n/a` with its reason, never as 0.

**LLM.** The measures are client latency and the ledger time `eacp.llm_calls.created_at` → `settled_at`. fakellm
answers immediately, so these figures are the gateway's overhead against a zero-latency provider. The report says in
words that real LLM latency is not measured.

**Percentiles.** Percentiles are exact nearest-rank over all measured samples. A step holds at most about 48,000
samples, so no sketch is needed.

### 3.5 Results and the report

- **Raw results.** They go to `bench-results/<UTC timestamp>-<sha7>-<pdp>.json` (git-ignored). The file holds:
  - schema version, git SHA, and whether the worktree was dirty;
  - host CPU model, core count and RAM; Docker's CPUs and memory; OS; Go, Docker and PostgreSQL versions;
  - the bench overrides and their defaults, and the flags;
  - setup durations;
  - per N and step: offered rate, sample counts, error and 429 counts, terminal tallies, every measure's
    P50/P95/P99/max, per-stage exclusions, resources, and the saturated and incomplete flags;
  - the maximum sustainable rate.
- **`eacp-bench report`.** It renders deterministic markdown from one or more results files:
  - a header with the date, SHA, machine, command and the SHA-256 of each input;
  - a caveats block;
  - per PDP, a table by N and step;
  - a summary of the maximum sustainable rate by PDP and N;
  - the LLM table.

  The caveats block states: one development machine; zero-latency fake ERP and LLM; the control-plane overhead,
  governance, external and LLM figures are separate; single tenant.
- **Baseline.** The first full run is committed as `docs/benchmarks/baseline-local.json` and
  `docs/benchmarks/baseline-agt.json`, and `docs/BENCHMARKS.md` is `eacp-bench report` of them. The README links to
  `docs/BENCHMARKS.md` and quotes at most the summary.

### 3.6 Honesty guards (§105), enforced by tests

- `TestBenchmarksDocMatchesBaseline`: `docs/BENCHMARKS.md` equals `report(docs/benchmarks/baseline-*.json)` byte for
  byte, so it can never be edited by hand.
- `TestReadmeNumbersComeFromTheBaseline`: every performance figure in `README.md` also appears in
  `docs/BENCHMARKS.md`. A performance figure is a number followed by `/s`, `ms`, `rps`, `req/s` or `actions/s`.
- Incomplete steps and `n/a` sources are rendered as such, never as numbers.

### 3.7 Safety

- Agent and admin keys are only in memory. The results schema has no field that can hold a key, and a test checks
  that a results file contains no generated key.
- The collector's SQL is read-only and runs only against the bench project's PostgreSQL port.
- `docker-compose.yml` and the development and demo stacks are unchanged.

## 4. Testing (TDD)

Unit tests in `internal/bench` need no stack:
- **Percentiles** on known data sets, including one sample and ties.
- **The scheduler** with a fake clock and a slow fake sender: latency is measured from the intended time and
  accumulates. The in-flight cap marks the step incomplete.
- **Stage computation** from synthetic transitions: the happy path, a retry (first attempt only), `DENIED`,
  `UNKNOWN_OUTCOME`, and a missing attempt that is excluded and counted.
- **The saturation rule**, each condition on both sides of its threshold.
- **Report**: a golden test, deterministic output, and `n/a` and incomplete rendering.
- **The two honesty guards** (§3.6), and the no-key check (§3.7).

An integration test with `EACP_TEST_ADMIN_DSN` runs the collector's SQL against actions driven through the registry
fixture to `SUCCEEDED`, `DENIED` and a retried outcome, and checks that every stage is read.

A smoke run, `scripts/bench.sh --quick`, on the real stack must pass before the full run. Everything also passes
under `go vet ./...` and `go test -race ./...` with PostgreSQL.

## 5. Expected findings and risks

- Every action transaction ends by locking the tenant's audit chain head (AGENTS.md lock order), so a single tenant's
  throughput is expected to plateau there. If the run shows it, the report says so as a design ceiling, and
  multi-tenant scaling is the next step.
- The AGT sidecar adds an mTLS hop and Python policy evaluation, so governance latency is expected to dominate
  admission under `microsoft-agt`.
- Docker Desktop's VM (8 CPUs, 8 GB here) may saturate before PostgreSQL does. The per-container CPU figures and the
  bench process's own CPU show which component saturates first.
- Setup for 10,000 agents is about 150,000 API calls (15 per agent). That is expected to take several minutes, and its duration is
  reported. A full run is estimated at 1–2 hours, an estimate to be replaced by the measured duration.

## 6. Done

- `internal/bench` and `cmd/eacp-bench` tests pass with `-race`, and the PostgreSQL integration test passes with
  `EACP_TEST_ADMIN_DSN`.
- `scripts/bench.sh --quick` passes.
- One full run has been made. Its baselines, `docs/BENCHMARKS.md` and the README link are committed.
- AGENTS.md (commands and layout), README and MASTER_PLAN §104 status are updated.
