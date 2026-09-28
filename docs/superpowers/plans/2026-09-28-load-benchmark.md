# Load Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Measure EACP end to end on the compose stack at 100–10,000 agents with open-loop load. Report
P50/P95/P99 for every MASTER_PLAN §104 measure, under both PDPs and for the LLM gateway, and commit the first real
run as `docs/BENCHMARKS.md`, generated from raw results.

**Architecture:**
- Pure logic lives in `internal/bench` and is testable without a stack: percentiles, the open-loop scheduler,
  per-stage computation, saturation, parsing of resource samples, the results schema and the markdown report.
- `internal/bench` also holds the read-only PostgreSQL collector, with an integration test.
- `cmd/eacp-bench` drives a live stack. `scripts/bench.sh` gives it a fresh, isolated compose project `eacp-bench`
  for each PDP.

**Tech Stack:** Go 1.27; pgx v5 (already in `go.mod`); docker compose; no new module dependencies.

**Spec:** `docs/superpowers/specs/2026-09-28-load-benchmark-design.md`

## Global Constraints

- The benchmark changes no service code and no `docker-compose.yml` service definition. Bench-only settings live in
  `deployments/bench/`.
- Every published port binds 127.0.0.1. The project name is `eacp-bench`: API 28080, gateway 28083, PostgreSQL
  55434.
- Bench overrides:
  - `EACP_WORKER_CONCURRENCY=32`;
  - `EACP_ACTION_MAX_QUEUED_PER_TENANT`, `EACP_ACTION_MAX_QUEUED_GLOBAL` and `EACP_ACTION_MAX_PENDING_PER_TENANT`
    are `100000`, with defaults 1000, 10000 and 1000;
  - PostgreSQL `track_functions=pl`;
  - the worker's default concurrency is 4.

  The report prints each override next to its default.
- Agents: N ∈ {100, 1000, 5000, 10000}, cumulative. Setup uses 16 concurrent workers.
- Action steps are 25, 50, 100, 200, 400 and 800 per second. LLM steps are 25, 50, 100 and 200 per second. Each step
  has a 10 s warm-up (excluded) and a 60 s measurement. The drain timeout is 5 min. The in-flight cap is 20,000.
- `--quick`: N = {100}, action steps {25, 50}, LLM steps {25}, a 5 s warm-up and a 20 s measurement.
- Saturation, where any condition is enough:
  - completed throughput is below 0.95 × offered;
  - (errors + 429s) exceed 0.01 × requests;
  - end-to-end P99 exceeds 5 s.

  Climbing stops at the first saturated or incomplete step.
- 5% of requests are idempotency replays: every request whose index satisfies `i % 20 == 19` replays request
  `i - 10`.
- Percentiles are exact nearest-rank: rank = ⌈p/100 · n⌉, then take the value at index rank−1 of the sorted
  samples.
- Terminal action states are SUCCEEDED, FAILED, DENIED, CANCELLED, EXPIRED and NEEDS_HUMAN_RESOLUTION.
- A missing measure is rendered as `n/a (<reason>)`, never 0. An incomplete step is rendered as `incomplete`.
- Keys (admin, person and agent) live only in process memory. They are never logged and never written; the results
  writer refuses bytes that contain one.
- The collector's SQL is read-only, and uses only `postgres://postgres:postgres@127.0.0.1:55434/eacp?sslmode=disable`
  (overridable with `--dsn`).
- Commit as the user only, with no Co-Authored-By trailer. Never use bare `git stash`.

## Rulings made while planning (spec deviations)

- **The bench tenant is `00000000-0000-4000-8000-0000000000be`, not `…00ac`.**
  - The spec assumed `…00ac` could use both the fake ERP and the fake LLM. The dev connector manifest has no ERP
    entry for `…00ac`, and no tenant has both.
  - Task 8 therefore adds `…00be` entries to `deployments/docker/secrets/connector-secrets.dev.json` (`fakeerp`,
    `fakeerp:8090`, the same value as the `…00d1` entry) and to `llm-secrets.dev.json` (`fakellm`, `fakellm:8093`,
    `dev-only-fakellm-key`). Both are development-only manifests.
  - Cost if wrong: two manifest lines.
- **NATS monitoring is read with `docker compose -p eacp-bench exec -T nats wget -qO- http://127.0.0.1:8222/<path>`,
  not a published port 28222.**
  - The `bus` network is `internal: true`, so a published port would not be reachable, and publishing one would
    weaken the boundary.
  - The healthcheck proves the image has `wget`.
  - Cost if wrong: the NATS columns show `n/a`.
- **The local PDP needs a second override, `deployments/bench/compose.bench-local.yml`.**
  - `EACP_GOVERNANCE_PROVIDER=local` with `EACP_AGT_PDP_URL` set is a startup error (`internal/config/config.go:250`).
  - The file sets the provider to `local` and blanks the four `EACP_AGT_PDP_*` variables. `get()` treats empty as
    unset.
- **Benchmark actions have a subject.** Actions whose subject is not an enabled human principal are denied (migration
  00005 resolves `subject_principal_id`). Setup therefore creates the human principal `sam` (`sam@bench.test`, no role),
  and every action uses that subject.

## Review Focus

1. A request that errors or times out at the client is kept as a sample with its latency and an error. It is never
   dropped, and a 429 is counted separately from other errors. (Task 2: `TestErrorsAndThrottlesAreKept`.)
2. A step whose actions never all reach a terminal state, for example one stuck in `UNKNOWN_OUTCOME`, ends at the
   drain timeout as `incomplete`. The run continues and reports; it never hangs. (Task 7: `TestDrainTimesOut`.)
3. Client and PostgreSQL clocks are never subtracted from each other. DB stages use only DB timestamps, and client
   latencies use only the client clock. (Task 3: `TestStagesUseOnlyDatabaseTimes`.)
4. A resource source that fails (docker, NATS or PostgreSQL) yields `n/a` with its reason, never zeros. (Task 5:
   `TestAFailedSourceIsNotZero`.)
5. A replay whose original has not yet been answered still gets exactly one action per key. The DB check counts
   actions per key after the drain. (Task 4: `TestDuplicateKeysAreCounted`.)

---

### Task 1: Percentiles and the saturation rule

**Files:**
- Create: `internal/bench/stats.go`, `internal/bench/saturation.go`
- Test: `internal/bench/stats_test.go`, `internal/bench/saturation_test.go`

**Interfaces:**
- Produces:
  - `type Summary struct { Count int; P50Ms, P95Ms, P99Ms, MaxMs float64 }`, with JSON tags `count`, `p50_ms`,
    `p95_ms`, `p99_ms` and `max_ms`.
  - `func Summarize(samples []time.Duration) Summary`. An empty input returns `Summary{}`. The input is not mutated.
  - `type Verdict struct { Throughput float64; Offered float64; Requests, Errors, Throttled int; EndToEndP99Ms float64 }`
  - `func SaturationReasons(v Verdict) []string`. Each reason is one of `"throughput"`, `"errors"` or `"p99"`. `nil`
    means not saturated.
  - The constants `ThroughputFloor = 0.95`, `ErrorCeiling = 0.01` and `P99Ceiling = 5 * time.Second`.

- [ ] **Step 1: Write the failing tests.**
  - `TestSummarizeNearestRank`: 1..100 ms gives P50 = 50, P95 = 95, P99 = 99 and Max = 100. A single sample of 7 ms
    gives 7 for every field. Ties: [1,1,1,9] ms gives P50 = 1 and P99 = 9. Empty gives `Summary{}`. The input slice
    order is unchanged afterwards.
  - `TestSaturationEachConditionBothSides`:
    - throughput 95 of 100 offered → no reason; 94.9 → `throughput`;
    - (errors + throttled) of 1 of 100 → none; 2 of 100 → `errors`;
    - P99 of 5000 ms → none; 5001 → `p99`;
    - all three at once → all three reasons, in that order.
- [ ] **Step 2:** Run `go test ./internal/bench/`. Expected: FAIL (undefined: Summarize).
- [ ] **Step 3:** Implement with the nearest-rank rule in Global Constraints. Sort a copy.
- [ ] **Step 4:** Run `go test -race ./internal/bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): percentiles and the saturation rule`.

### Task 2: The open-loop scheduler and replays

**Files:**
- Create: `internal/bench/loadgen.go`
- Test: `internal/bench/loadgen_test.go`

**Interfaces:**
- Consumes: `Summarize` (Task 1).
- Produces:
  - `type Clock interface { Now() time.Time; SleepUntil(ctx context.Context, t time.Time) error }`, and
    `RealClock{}` implementing it.
  - `type Plan struct { Rate float64; Warmup, Measure time.Duration; MaxInFlight int }`
  - `type Response struct { Status int; Err error; ActionID string }`
  - `type Sample struct { Index int; Intended time.Time; Latency time.Duration; Status int; Err string; Measured, Replay bool; ActionID string }`
  - `type Load struct { Samples []Sample; Incomplete bool; Started, Ended time.Time }`. `Started` and `Ended` bound
    the measurement window: `Started` is t0 + Warmup and `Ended` is `Started` + Measure.
  - `func RunOpenLoop(ctx context.Context, clk Clock, p Plan, send func(ctx context.Context, i int) Response) Load`.
    Request i's intended time is t0 + i/Rate. It is sent in its own goroutine, and latency = `clk.Now()` at response
    − intended. `Measured` means intended ≥ t0 + Warmup. The loop stops scheduling at t0 + Warmup + Measure, waits
    for all responses, and marks the load `Incomplete` if in-flight would exceed `MaxInFlight`; in that case it stops
    sending.
  - `func IsReplay(i int) bool` (`i%20 == 19`) and `func ReplayOf(i int) int` (`i-10`).
  - `func Counts(samples []Sample) (requests, errors, throttled int)`. It counts only measured samples. 429 is
    throttled; any other non-2xx status or `Err` is an error.

- [ ] **Step 1: Write the failing tests.** They use a fake clock where `SleepUntil` advances time and `send` advances
  the clock by a scripted service time.
  - `TestLatencyIsMeasuredFromTheIntendedTime`: rate 10/s over 1 s, with the server taking 500 ms for request 0 and
    0 afterwards. With a serialized fake server, request 1's latency is ≥ 400 ms: latency accumulates and is not
    hidden.
  - `TestWarmupIsExcluded`: a 1 s warm-up and 1 s measurement at 10/s gives 10 measured samples out of 20.
  - `TestTheInFlightCapMarksIncomplete`: `MaxInFlight` 3 with a server that never answers until the context ends
    gives `Incomplete == true`.
  - `TestErrorsAndThrottlesAreKept`: statuses [201, 429, 500, 0 with an Err] give 4 samples, `Counts` = (4, 2, 1),
    and every sample has a latency > 0.
  - `TestReplaySelection`: `IsReplay` is true exactly for 19, 39 and 59 among 0..59, and `ReplayOf(19) == 9`.
- [ ] **Step 2:** Run `go test ./internal/bench/ -run 'Latency|Warmup|InFlight|Throttles|Replay'`. Expected: FAIL.
- [ ] **Step 3:** Implement `RunOpenLoop` with a `sync.WaitGroup` and a mutex-guarded `[]Sample`. Replays are marked
  by `IsReplay(i)`.
- [ ] **Step 4:** Run `go test -race ./internal/bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): the open-loop scheduler`.

### Task 3: Per-stage computation

**Files:**
- Create: `internal/bench/stages.go`
- Test: `internal/bench/stages_test.go`

**Interfaces:**
- Consumes: `Summary` and `Summarize` (Task 1).
- Produces:
  - `type Transition struct { From, To string; At time.Time }`. `From` is `""` for `action.received`.
  - `type Timeline struct { ActionID string; Transitions []Transition; Dispatched, Completed *time.Time }`. These are
    the first attempt's times, and the transitions are in journal order.
  - `type Stages struct { Governance, QueueWait, Lease, External, Overhead, EndToEnd []time.Duration; Excluded map[string]int; Terminal map[string]int; Open int }`
  - `func ComputeStages(ts []Timeline) Stages`, with the spec §3.4 definitions:
    - governance: received → the first transition with `From == "RECEIVED"`;
    - queue wait: the first `To == "QUEUED"` → the first `To == "LEASED"`;
    - lease: the first `LEASED` → `Dispatched`;
    - external: `Dispatched` → `Completed`;
    - end-to-end: received → the first transition into a terminal state;
    - overhead: end-to-end − governance − external. It exists only when all three exist.

    A stage that cannot be computed increments `Excluded[stage]`. `Terminal[state]` counts the final terminal state.
    An action with none counts in `Open`.
  - `var TerminalStates = []string{"SUCCEEDED", "FAILED", "DENIED", "CANCELLED", "EXPIRED", "NEEDS_HUMAN_RESOLUTION"}`

- [ ] **Step 1: Write the failing tests.**
  - `TestHappyPathStages`: received at t, RECEIVED→QUEUED at +10 ms (released), QUEUED→LEASED at +30, LEASED→EXECUTING
    at +31, Dispatched at +32, Completed at +40, EXECUTING→SUCCEEDED at +45. This gives governance 10, queue wait 20,
    lease 2, external 8, end-to-end 45, overhead 27, and `Terminal["SUCCEEDED"] == 1`.
  - `TestARetryCountsTheFirstAttempt`: two LEASED transitions and a RETRY_WAIT; queue wait and lease use the first
    LEASED.
  - `TestADeniedActionHasOnlyGovernanceAndEndToEnd`: RECEIVED→DENIED at +5 gives governance 5 and end-to-end 5. It
    adds one exclusion each to queue_wait, lease, external and overhead.
  - `TestAnUnknownOutcomeIsOpen`: an action ending in UNKNOWN_OUTCOME gives `Open == 1` and excludes end_to_end.
  - `TestStagesUseOnlyDatabaseTimes`: `Timeline` has no client-time field. The test compiles a `Timeline` literal
    with every field set and asserts `reflect.TypeOf(Timeline{}).NumField() == 4`. That locks the struct, so adding a
    client time fails the test.
- [ ] **Step 2:** Run `go test ./internal/bench/ -run Stages`. Expected: FAIL.
- [ ] **Step 3:** Implement `ComputeStages`.
- [ ] **Step 4:** Run `go test -race ./internal/bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): per-stage timings from the journal`.

### Task 4: The PostgreSQL collector

**Files:**
- Create: `internal/bench/collect.go`
- Test: `internal/bench/collect_test.go` (package `bench_test`; skipped without `EACP_TEST_ADMIN_DSN`, like every
  `registrytest` user)

**Interfaces:**
- Consumes: `Timeline` and `Transition` (Task 3).
- Produces. Every function runs read-only SQL on a superuser pool, which bypasses RLS:
  - `func ReadTimelines(ctx context.Context, db *pgxpool.Pool, tenant uuid.UUID, ids []uuid.UUID) ([]Timeline, error)`
    - It reads `eacp.audit_events`, using `convert_from(payload, 'UTF8')::jsonb`, where the action is `action.received`
      or `action.transition` and `subject.id = ANY(ids)`. `From`/`To` come from `data.from`/`data.to`, and `At` is
      `recorded_at`.
    - It joins `eacp.action_attempts` where `attempt_no = 1`.
    - Timelines come back ordered by `seq`.
  - `func OpenCount(ctx, db, tenant uuid.UUID, ids []uuid.UUID) (int, error)`: how many of ids are not in a terminal
    state.
  - `func DuplicateKeys(ctx, db, tenant uuid.UUID, since time.Time) (int, error)`: the number of
    `(agent_id, idempotency_key)` pairs with more than one action. It is always 0 while the unique constraint stands;
    it guards a regression.
  - `type DBSample struct { At time.Time; Commits int64; BlksHit, BlksRead int64; Active, LockWaits int; SizeBytes int64 }`
    and `func ReadDB(ctx, db) (DBSample, error)`. Commits are `xact_commit` for datname `eacp`. `Active` and
    `LockWaits` come from `pg_stat_activity`: state `active`, and `wait_event_type = 'Lock'`.
  - `func ReadFunctionStats(ctx, db, name string) (calls int64, totalMs float64, err error)` from
    `pg_stat_user_functions`, where `schemaname = 'eacp'` and `funcname = name`.
  - `func ReadLLMCalls(ctx, db, tenant uuid.UUID, since, until time.Time) ([]time.Duration, error)`:
    `settled_at - created_at` for calls created in the window and settled.

- [ ] **Step 1: Write the failing test** `TestReadTimelinesReadsEveryStage` with `registrytest.New(t)`.
  - Set up: `ActiveTool` erp/purchase, `ActivatePolicy(registrytest.AllowPolicy)`, `ActiveAgent`, then
    `QueuedAction`.
  - Drive a with `worker.NewStore(f.App, "bench")`: `Claim` → `Intent(ctx, l, 2*time.Second)` →
    `Complete(ctx, l, worker.Result{Outcome: worker.Succeeded, ExternalReference: "po-1"}, 100*time.Millisecond)`.
  - Create a second `QueuedAction` b and leave it queued.
  - Assert:
    - `ComputeStages(ReadTimelines(f.Owner-pool, …, [a, b]))` gives `Terminal["SUCCEEDED"] == 1` and `Open == 1`;
    - a has governance, queue wait, lease, external and end-to-end values ≥ 0 (one sample each);
    - `OpenCount == 1`;
    - `ReadDB` has `Commits > 0`.

  Use the fixture's owner pool, since the fixture's superuser pool reads through RLS. The implementer checks
  `registrytest.Fixture` for its admin pool field; if the only superuser handle is `pgtest`'s admin DSN, the test
  opens `pgxpool.New(ctx, os.Getenv("EACP_TEST_ADMIN_DSN"))` against the fixture database.
- [ ] **Step 2: Write the failing test** `TestDuplicateKeysAreCounted`: it asserts 0 after the actions above.
- [ ] **Step 3:** Run `go test ./internal/bench/ -run 'ReadTimelines|DuplicateKeys'` with `EACP_TEST_ADMIN_DSN`
  set. Expected: FAIL (undefined).
- [ ] **Step 4:** Implement `collect.go`.
- [ ] **Step 5:** Run `go test -race ./internal/bench/` with the DSN. Expected: PASS, not SKIP (check with `-v`).
- [ ] **Step 6:** Add every new SQL touchpoint to `internal/storage/rls_catalog_test.go` only if that test demands it.
  It demands it only for new tables or functions, and none are created here. Commit
  `feat(bench): the read-only PostgreSQL collector`.

### Task 5: Resource samples

**Files:**
- Create: `internal/bench/resources.go`
- Test: `internal/bench/resources_test.go`

**Interfaces:**
- Produces:
  - `type ContainerSample struct { Service string; CPUPercent float64; MemBytes int64 }`
  - `func ParseDockerStats(line []byte, project string) (ContainerSample, error)`. The input is one
    `docker stats --no-stream --format json` line. The keys `Name` (e.g. `eacp-bench-controlplane-api-1` gives service
    `controlplane-api`), `CPUPerc` (`"12.34%"`) and `MemUsage` (`"123.4MiB / 7.7GiB"`, with units B, KiB, MiB and
    GiB) were verified on this machine.
  - `type NATSSample struct { At time.Time; InMsgs, OutMsgs, InBytes, OutBytes int64 }` and
    `func ParseVarz(body []byte, at time.Time) (NATSSample, error)`, with the keys `in_msgs`, `out_msgs`, `in_bytes`
    and `out_bytes`.
  - `type Resource struct { MeanCPU, MaxCPU float64; MaxMemBytes int64; NA string }`, where a non-empty `NA` is the
    reason the source failed.
  - `func AggregateContainers(samples []ContainerSample, failures []string) map[string]Resource`. If no samples
    exist, every service in `KnownServices` gets `NA` set to the first failure, or to `"no samples"`.
  - `var KnownServices = []string{"controlplane-api", "execution-worker", "agt-pdp", "postgres", "nats", "fakeerp", "llm-gateway", "fakellm"}`
  - `type Rates struct { PerSecond map[string]float64; NA string }`
  - `func DBRates(first, last DBSample) Rates`, with keys `commits_per_s`, `cache_hit_ratio`, `max_active` and
    `max_lock_waits`. The max keys come from a separate `MaxActivity(samples []DBSample)`.
  - `func NATSRates(first, last NATSSample) Rates`, with keys `in_msgs_per_s`, `out_msgs_per_s`, `in_bytes_per_s`
    and `out_bytes_per_s`.

- [ ] **Step 1: Write the failing tests.**
  - `TestParseDockerStats`: two real-format lines, one with `MiB` and one with `GiB`.
  - `TestParseVarz`: a JSON fixture.
  - `TestRatesFromDeltas`: two DB samples 10 s apart with a commits delta of 500 give `commits_per_s == 50`.
  - `TestAFailedSourceIsNotZero`: `AggregateContainers(nil, ["docker: not found"])` gives `NA == "docker: not found"`
    for every known service, and `MaxCPU == 0` is never rendered (checked in Task 6).
- [ ] **Step 2:** Run `go test ./internal/bench/ -run 'Docker|Varz|Rates|FailedSource'`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test -race ./internal/bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): container, database and NATS samples`.

### Task 6: The results schema, the guarded writer and the report

**Files:**
- Create: `internal/bench/results.go`, `internal/bench/report.go`, `internal/bench/testdata/results-local.json`,
  `internal/bench/testdata/results-agt.json`, `internal/bench/testdata/report.golden`
- Test: `internal/bench/results_test.go`, `internal/bench/report_test.go`

**Interfaces:**
- Consumes: `Summary`, `Resource`, `Rates` and `Stages` (Tasks 1, 3 and 5).
- Produces:
  - `const SchemaVersion = 1`
  - `type Results struct { Schema int; GitSHA string; Dirty bool; PDP string; StartedAt time.Time; Machine Machine; Overrides []Override; Flags map[string]string; Setup []SetupTiming; Levels []Level; LLM []Step; MaxSustainable map[int]float64 }`
    - JSON keys are snake_case.
    - `MaxSustainable` maps an N to a rate, where 0 means that no step was sustainable.
  - `type Machine struct { HostCPU string; HostCores int; HostMemBytes int64; DockerCPUs int; DockerMemBytes int64; OS, Go, Docker, Postgres string }`
  - `type Override struct { Name, Value, Default string }`
  - `type SetupTiming struct { Agents int; Seconds float64 }`
  - `type Level struct { Agents int; Steps []Step }`
  - `type Step struct { Offered float64; Requests, Errors, Throttled int; AdmissionThroughput, CompletedThroughput float64; Admission, Idempotency, Governance, QueueWait, Lease, External, Overhead, EndToEnd, LLMClient, LLMLedger Summary; Excluded, Terminal map[string]int; BudgetReserveMeanMs *float64; Containers map[string]Resource; DB, NATS Rates; Saturated []string; Incomplete bool; NA map[string]string }`
  - `func WriteResults(path string, r Results, secrets []string) error`. It marshals with indentation and refuses,
    before writing anything, if any non-empty secret is a substring of the bytes: `errors.New("results contain a
    secret")`. The file is written 0o644.
  - `type Input struct { Name string; Raw []byte }`
  - `const GeneratedBegin = "<!-- generated by eacp-bench report: do not edit -->"` and
    `const GeneratedEnd = "<!-- end generated -->"`.
  - `func Splice(doc, block string) string` replaces the text between the markers, inclusive, with block. When doc
    has no markers it returns block + "\n". Hand-written text around the block survives regeneration.
  - `func Report(inputs []Input) (string, error)`. It returns the block wrapped in the markers. The output is
    deterministic; map keys and levels are sorted. It contains:
    - the header: generation date = the latest `StartedAt`, the git SHA, the machine, the command
      `scripts/bench.sh`, and `sha256(Raw)` for each input by `Name`;
    - the caveats block, which is exact copy (the next bullet);
    - per PDP, a table: `| Agents | Offered/s | Admission P50/P95/P99 | Governance | Queue wait | Lease |
      External | Overhead | End-to-end | Idempotency | Budget reserve (mean) | Completed/s | Errors | 429 |
      Status |`;
    - per PDP and N, a resources table;
    - a summary table of the max sustainable rate (PDP × N);
    - the LLM table.

    Status is `ok`, `saturated (<reasons>)` or `incomplete`. A `Summary` with `Count == 0` renders `n/a`, and so
    does a `Resource` with `NA` (plus its reason). Percentiles render as `%.1f` ms.
  - The caveats copy: "Measured on one development machine (details above). The ERP and the LLM are local fakes
    that answer immediately, so External and LLM figures are the fakes' own time, not a real system's. Overhead is
    end-to-end minus governance minus external: the control plane's own time. One tenant. These figures are not a
    capacity claim for production hardware."

- [ ] **Step 1: Write the failing tests.**
  - `TestWriteResultsRefusesASecret`: writing `Results{Flags: {"x": "eacp_sk_abc"}}` with secrets
    `["eacp_sk_abc"]` returns an error and creates no file. With secrets `["other"]`, the file is written and
    round-trips to an equal `Results`.
  - `TestReportGolden`: `Report` of the two testdata inputs equals `testdata/report.golden`. Regenerate it with
    `-update` only when the change is intended. The testdata includes one incomplete step, one saturated step, one
    `n/a` container and one `Count == 0` summary.
  - `TestReportIsDeterministic`: two calls give equal output.
  - `TestSpliceKeepsTheText`: splicing a new block into `"A\n" + old block + "\nFindings"` gives
    `"A\n" + new block + "\nFindings"`. A doc with no markers gives block + "\n".
  - `TestNotAvailableIsNeverZero`: no `n/a` input renders as `0.0` or `0` in its cell.
- [ ] **Step 2:** Run `go test ./internal/bench/ -run 'WriteResults|Report|NotAvailable'`. Expected: FAIL.
- [ ] **Step 3:** Implement. Write the testdata JSON by hand; they are small, with 2 levels × 2 steps. Generate the
  golden with `go test ./internal/bench -run TestReportGolden -update` and read it in full before committing.
- [ ] **Step 4:** Run `go test -race ./internal/bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): results, the guarded writer and the markdown report`.

### Task 7: `cmd/eacp-bench`

**Files:**
- Create:
  - `cmd/eacp-bench/main.go` (flags and subcommands);
  - `cmd/eacp-bench/client.go` (JSON over HTTP, with keys in memory);
  - `cmd/eacp-bench/setup.go` (tenant, people, registry and agents);
  - `cmd/eacp-bench/traffic.go` (action and LLM senders);
  - `cmd/eacp-bench/sampler.go` (docker, PostgreSQL and NATS samplers);
  - `cmd/eacp-bench/run.go` (levels, steps, drain and collection);
  - `cmd/eacp-bench/machine.go`.
- Test: `cmd/eacp-bench/run_test.go`, `cmd/eacp-bench/main_test.go`

**Interfaces:**
- Consumes: every `internal/bench` export above; `identity.NewKey(kind, tenant, credential)`, as in
  `test/demo/demo_test.go:304`.
- Produces:
  - The CLI:
    - `eacp-bench run --pdp local|microsoft-agt [--quick] [--erp-delay-ms N] [--api http://127.0.0.1:28080] [--gateway http://127.0.0.1:28083] [--dsn …55434/eacp] [--project eacp-bench] [--compose-files a,b,c] [--out bench-results]`
    - `eacp-bench report [--out docs/BENCHMARKS.md] file.json...` (with `--out`, the file is rewritten with `Splice`, keeping hand-written text)
  - `type config struct` holds the resolved levels, rates and durations. `func resolve(quick bool) config` returns
    the Global Constraints values.
  - `func drain(ctx context.Context, open func(context.Context) (int, error), timeout, every time.Duration) (incomplete bool, err error)`
- Behaviour, following spec §3.2 and §3.3 and this plan's rulings:
  - **Tenant.** Setup creates tenant `…00be` with `docker compose -p <project> -f … run --rm migrate /eacpctl tenant
    create --id … --slug bench --name Bench --admin name=alice,… --admin name=bob,…`, as
    `test/demo/platform_test.go:81` does.
  - **People.** erin (`registry_editor`), rita and ravi (`registry_approver`) and sam (no role). Each gets a key
    approved by bob, as in `tenantWithCast`.
  - **Policy.** Two allow rules: `{"id":"erp","match":{"target":"erp"},"verdict":"allow","reason":"benchmark"}` and
    `{"id":"llm","match":{"operation":"llm.generate"},"verdict":"allow","reason":"benchmark"}`.
  - **ERP.** Connector `erp` (`http://fakeerp:8090`, `secret_ref` `fakeerp`) with tool `create_po`. The contract is
    the demo's `create_po` contract (`test/demo/demo_test.go:371`) plus `cost_unit` `THB`, `cost_amount_field`
    `amount` and `cost_unit_field` `currency`, approved by rita.
  - **LLM.** Model `sonnet` (anthropic, `http://fakellm:8093`, upstream `claude-fake`, `secret_ref` `fakellm`,
    `max_output_tokens` 100000). alice sets its price at 3/15 USD per MTok.
  - **Agents.** 16 workers create agents `bench-00001`…:
    - agent (owner: sam), version, allowlist `{"tools":["erp.create_po"],"models":["sonnet"]}` approved by rita,
      then ACTIVE by ravi;
    - an agent key proposed by erin and approved by rita;
    - a THB account with a limit of `1000000000` and a USD account with a limit of `1000000`, each proposed by alice
      and approved by bob.
  - **Action request.** `POST /v1/actions` with the agent's key as Bearer and `Idempotency-Key`
    `b-<level>-<step>-<i>` (replays reuse `ReplayOf(i)`'s key and agent). The body is `subject` `sam@bench.test`,
    `operation` `purchase`, `target` `erp`, `tool` `erp.create_po`, `tool_schema_version` `"1"`, `resource` `po` and
    `payload` `{"amount":1,"currency":"THB"}`, plus `delay_ms` if the flag is set. Statuses 200, 201 and 202 are
    accepted.
  - **LLM request.** `POST /v1/messages` with the headers `x-api-key` and `anthropic-version: 2023-06-01` and the body
    `{"model":"sonnet","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"benchmark"}]}`. Only 200
    is accepted.
  - **Samplers.** They run every 2 s during the measurement only:
    - `docker stats --no-stream --format json`, filtered to `<project>-`;
    - `ReadDB`;
    - NATS `/varz` through `docker compose exec -T nats wget -qO- http://127.0.0.1:8222/varz`.

    A sampler error is recorded as that source's `NA` reason. The bench process's own CPU comes from
    `syscall`-free `runtime/metrics` `/cpu/classes/user:cpu-seconds` deltas, reported as the container `eacp-bench`.
  - **Around each step.** Before the step, `ReadFunctionStats("budget_reserve")`; after the drain, again. The mean is
    the delta of `total_ms` over the delta of `calls`, or nil when the calls delta is 0.
  - **After each action step.** Drain with `OpenCount` every 1 s and a 5 min timeout. Then `ReadTimelines` →
    `ComputeStages`, `DuplicateKeys`, the saturation reasons and the throughput. Completed throughput is the number of
    terminal actions among the measured ids ÷ the measurement seconds.
  - **Results.** They are written with `WriteResults(path, r, allKeys)` to
    `<out>/<UTC 20060102T150405Z>-<sha7>-<pdp>.json`.
- [ ] **Step 1: Write the failing tests.**
  - `TestResolveQuickAndFull`: the full config gives levels [100, 1000, 5000, 10000], action rates
    [25, 50, 100, 200, 400, 800], LLM rates [25, 50, 100, 200], a 10 s warm-up, a 60 s measurement and a 5 min drain.
    Quick gives [100], [25, 50] and [25], with 5 s and 20 s.
  - `TestDrainTimesOut`: an `open` func that always returns 3 gives `incomplete == true` after the timeout, using a
    50 ms timeout and a 10 ms interval. One that returns 0 on its third call gives false.
  - `TestClimbStopsAtTheFirstBadStep`: `climb(rates, stepFn)`, where stepFn reports saturated at 100, runs 25, 50
    and 100 and returns max sustainable 50. If every step is saturated, it returns 0.
- [ ] **Step 2:** Run `go test ./cmd/eacp-bench/`. Expected: FAIL.
- [ ] **Step 3:** Implement the files above. `climb(rates []float64, step func(rate float64) bench.Step) (steps []bench.Step, maxSustainable float64)`
  lives in `run.go`.
- [ ] **Step 4:** Run `go vet ./cmd/eacp-bench/ && go test -race ./cmd/eacp-bench/`. Expected: PASS.
- [ ] **Step 5:** Commit `feat(bench): eacp-bench drives the stack and writes results`.

### Task 8: Stack wiring, `scripts/bench.sh` and the smoke run

**Files:**
- Create: `deployments/bench/compose.bench.yml`, `deployments/bench/compose.bench-local.yml`, `scripts/bench.sh`
- Modify: `deployments/docker/secrets/connector-secrets.dev.json` and `deployments/docker/secrets/llm-secrets.dev.json`
  (add the `…00be` entries, per the rulings), and `.gitignore` (add `/bench-results/`).

**Interfaces:**
- Consumes: the `eacp-bench run` and `report` CLI (Task 7).
- Produces:
  - `scripts/bench.sh [--quick]`, which honours `KEEP=1` and `PDPS` (default `local microsoft-agt`). For each PDP:
    - run `compose down -v`, then `up -d --build` with `-f docker-compose.yml -f deployments/bench/compose.bench.yml`
      (plus `compose.bench-local.yml` for `local`);
    - wait for `/readyz` on 28080 and 28083;
    - run `go run ./cmd/eacp-bench run --pdp …`;
    - run `down -v` unless `KEEP=1`.

    It uses `MSYS_NO_PATHCONV=1` and `prepare_fakeerp_token.py`, as `demo.sh` does, and prints the results paths at
    the end.
  - `compose.bench.yml`:
    - ports `!override` for postgres (`127.0.0.1:55434:5432`), controlplane-api (`127.0.0.1:28080:8080`) and
      llm-gateway (`127.0.0.1:28083:8083`);
    - on the API, the three admission limits set to `"100000"`;
    - on the worker, `EACP_WORKER_CONCURRENCY: "32"`;
    - postgres `command: ["postgres", "-c", "track_functions=pl"]`.
  - `compose.bench-local.yml`: the API with `EACP_GOVERNANCE_PROVIDER: local` and the four `EACP_AGT_PDP_*` set to
    `""`.

- [ ] **Step 1:** Run `docker compose -p eacp-bench -f docker-compose.yml -f deployments/bench/compose.bench.yml -f deployments/bench/compose.bench-local.yml config`
  and read the rendered API environment. Expected: the provider is `local`, the AGT variables are empty, and the
  ports are on 127.0.0.1.
- [ ] **Step 2:** Run `go test ./...`, limited to any test that loads the dev manifests, with `grep -rl
  "connector-secrets.dev.json\|llm-secrets.dev.json" --include=*_test.go`. Expected: PASS after the manifest edits.
- [ ] **Step 3:** Run `bash scripts/bench.sh --quick`, with output going to a file in the workspace. Read the tail.
  Expected:
  - two results files;
  - every N = 100 step shows `Terminal["SUCCEEDED"] == measured requests - replays`, 0 errors and 0 duplicate keys;
  - `budget_reserve` has a mean;
  - the LLM step shows 200s;
  - `eacp-bench report` on both files renders.

  If a figure is `n/a`, find the cause (the systematic-debugging skill) before continuing.
- [ ] **Step 4:** Check that no generated key appears in either results file: the writer refuses such a file, so the
  presence of the files is the evidence.
- [ ] **Step 5:** Commit `feat(bench): the isolated bench stack and scripts/bench.sh`.

### Task 9: The full run, the baseline, the honesty guards and the docs

**Files:**
- Create: `docs/benchmarks/baseline-local.json`, `docs/benchmarks/baseline-agt.json`, `docs/BENCHMARKS.md`
  (generated), `internal/bench/docs_test.go`
- Modify: `README.md` (a Benchmarks section linking `docs/BENCHMARKS.md` and quoting at most the summary),
  `AGENTS.md` (Commands: `scripts/bench.sh`; Layout: `internal/bench`, `cmd/eacp-bench`, `deployments/bench`,
  `docs/benchmarks`; the Rules line from §105), `docs/MASTER_PLAN.md` §104 (a Status line) and
  `docs/superpowers/specs/2026-09-28-load-benchmark-design.md` (the rulings, where they changed the spec).

**Interfaces:**
- Consumes: `Report` and `Input` (Task 6); `scripts/bench.sh` (Task 8).

- [ ] **Step 1: Write the failing tests** in `internal/bench/docs_test.go`.
  - `TestBenchmarksDocMatchesBaseline`: reads `../../docs/benchmarks/baseline-*.json` (sorted) and expects
    `Report(inputs)` to equal, byte for byte, the marked block of `../../docs/BENCHMARKS.md` (markers included). It
    fails, not skips, when a file or a marker is missing.
  - `TestReadmeNumbersComeFromTheBaseline`: finds `\d[\d,.]*\s*(?:/s|ms|rps|req/s|actions/s)\b` in `../../README.md`.
    Every match's number must appear in `docs/BENCHMARKS.md`.
- [ ] **Step 2:** Run `go test ./internal/bench/ -run 'Doc|Readme'`. Expected: FAIL (no baseline).
- [ ] **Step 3:** Run `bash scripts/bench.sh` in the background, with output going to a workspace file, and wait
  for it to finish. Do not edit the scripts while it runs. Read the tail. Both results files must exist.
- [ ] **Step 4:** Copy the two results files to `docs/benchmarks/baseline-local.json` and `baseline-agt.json`
  unchanged. Run `go run ./cmd/eacp-bench report --out docs/BENCHMARKS.md docs/benchmarks/baseline-agt.json
  docs/benchmarks/baseline-local.json`, then read `docs/BENCHMARKS.md` in full.
- [ ] **Step 5:** Write the README Benchmarks section, quoting only numbers present in `BENCHMARKS.md`, and the
  AGENTS.md, MASTER_PLAN and spec updates. Describe what the run actually showed, for example where it saturated
  and which container was busiest, in `BENCHMARKS.md`'s hand-written "Findings" section.
  - The findings sit below the generated block. They state figures only by quoting cells from the block, and
    `report --out` keeps them (`Splice`).
- [ ] **Step 6:** Run `go vet ./... && go test -race ./...` with `EACP_TEST_ADMIN_DSN` set, plus
  `EACP_HELM_REQUIRED=1 go test ./test/helm` and `go test ./test/invariants`. Expected: all PASS, with the PostgreSQL
  tests not skipped.
- [ ] **Step 7:** Commit `docs: the first measured load benchmark (MASTER_PLAN §104)`.
