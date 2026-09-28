package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/bench"
)

type options struct {
	pdp          string
	quick        bool
	erpDelayMS   int
	api          string
	gateway      string
	dsn          string
	project      string
	composeFiles []string
	out          string
}

// config is what one run measures (spec §3.3).
type config struct {
	levels      []int
	actionRates []float64
	llmRates    []float64
	warmup      time.Duration
	measure     time.Duration
	drain       time.Duration
	maxInFlight int
}

func resolve(quick bool) config {
	if quick {
		return config{levels: []int{100}, actionRates: []float64{25, 50}, llmRates: []float64{25},
			warmup: 5 * time.Second, measure: 20 * time.Second, drain: 5 * time.Minute, maxInFlight: 20000}
	}
	return config{levels: []int{100, 1000, 5000, 10000}, actionRates: []float64{25, 30, 35, 40, 45, 50, 100, 200, 400, 800},
		llmRates: []float64{25, 50, 100, 200}, warmup: 10 * time.Second, measure: 60 * time.Second,
		drain: 5 * time.Minute, maxInFlight: 20000}
}

// overrides are the bench-only settings of deployments/bench, next to their
// defaults (TestOverridesMatchTheComposeFiles keeps them in step).
func overrides(pdp string) []bench.Override {
	o := []bench.Override{
		{Name: "EACP_WORKER_CONCURRENCY", Value: "32", Default: "4"},
		{Name: "EACP_ACTION_MAX_QUEUED_PER_TENANT", Value: "100000", Default: "1000"},
		{Name: "EACP_ACTION_MAX_QUEUED_GLOBAL", Value: "100000", Default: "10000"},
		{Name: "EACP_ACTION_MAX_PENDING_PER_TENANT", Value: "100000", Default: "1000"},
		{Name: "track_functions", Value: "pl", Default: "none"},
	}
	if pdp == "local" {
		o = append(o, bench.Override{Name: "EACP_GOVERNANCE_PROVIDER", Value: "local", Default: "microsoft-agt (compose)"})
	}
	return o
}

// drain polls open every interval until it reports 0 (false) or timeout
// passes (true: the step is incomplete).
func drain(ctx context.Context, open func(context.Context) (int, error), timeout, every time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		n, err := open(ctx)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
		if time.Now().After(deadline) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(every):
		}
	}
}

// climb runs rates in order and stops after the first saturated or
// incomplete step; best is the highest rate before it (0 if none).
func climb(rates []float64, step func(rate float64) bench.Step) (steps []bench.Step, best float64) {
	for _, rate := range rates {
		s := step(rate)
		steps = append(steps, s)
		if s.Incomplete || len(s.Saturated) > 0 {
			break
		}
		best = rate
	}
	return steps, best
}

type runner struct {
	o   options
	cfg config
	s   *setup
	db  *pgxpool.Pool
	gw  *client
}

func run(ctx context.Context, o options) (string, error) {
	cfg := resolve(o.quick)
	db, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	api := newClient(o.api)
	rn := &runner{o: o, cfg: cfg, s: &setup{o: o, api: api}, db: db, gw: newClient(o.gateway)}
	sha, dirty := git(ctx)
	started := time.Now().UTC()
	r := bench.Results{Schema: bench.SchemaVersion, GitSHA: sha, Dirty: dirty, PDP: o.pdp, StartedAt: started,
		Machine: machine(ctx, db), Overrides: overrides(o.pdp),
		Flags:          map[string]string{"quick": strconv.FormatBool(o.quick), "erp_delay_ms": strconv.Itoa(o.erpDelayMS)},
		MaxSustainable: map[int]float64{}}
	path := filepath.Join(o.out, fmt.Sprintf("%s-%s-%s.json", started.Format("20060102T150405Z"), sha, o.pdp))
	save := func() error { return bench.WriteResults(path, r, api.secrets()) }

	logf("setup: tenant %s, people and registry", benchTenant)
	if err := rn.s.tenant(ctx); err != nil {
		return "", err
	}
	if err := rn.s.registry(ctx); err != nil {
		return "", err
	}
	for li, n := range cfg.levels {
		began := time.Now()
		if err := rn.s.grow(ctx, n); err != nil {
			return "", err
		}
		secs := time.Since(began).Seconds()
		r.Setup = append(r.Setup, bench.SetupTiming{Agents: n, Seconds: secs})
		logf("setup: %d agents in %.0fs", n, secs)
		steps, best := climb(cfg.actionRates, func(rate float64) bench.Step { return rn.actionStep(ctx, li, rate) })
		r.Levels = append(r.Levels, bench.Level{Agents: n, Steps: steps})
		r.MaxSustainable[n] = best
		if err := save(); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return path, ctx.Err()
		}
	}
	r.LLM, _ = climb(cfg.llmRates, func(rate float64) bench.Step { return rn.llmStep(ctx, rate) })
	return path, save()
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05 ")+format+"\n", args...)
}

func (rn *runner) dbNow(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := rn.db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&t)
	return t, err
}

// measured runs load with the samplers active during its measurement
// window, and returns that window in database time.
func (rn *runner) measured(ctx context.Context, rate float64, send func(context.Context, int) bench.Response) (
	load bench.Load, sm *sampler, from, to time.Time, err error) {
	sm = &sampler{s: rn.s, db: rn.db}
	start := time.Now().Add(rn.cfg.warmup)
	done := make(chan error, 1)
	go func() {
		if err := (bench.RealClock{}).SleepUntil(ctx, start); err != nil {
			done <- err
			return
		}
		var err error
		if from, err = rn.dbNow(ctx); err != nil {
			done <- err
			return
		}
		sctx, cancel := context.WithDeadline(ctx, start.Add(rn.cfg.measure))
		sm.run(sctx)
		cancel()
		to, err = rn.dbNow(ctx)
		done <- err
	}()
	load = bench.RunOpenLoop(ctx, bench.RealClock{}, bench.Plan{Rate: rate, Warmup: rn.cfg.warmup,
		Measure: rn.cfg.measure, MaxInFlight: rn.cfg.maxInFlight}, send)
	err = <-done
	return load, sm, from, to, err
}

// settle waits (at most the drain timeout) until the tenant has no open
// actions, so a step does not inherit an earlier step's backlog, and
// returns how many were still open when it gave up.
func (rn *runner) settle(ctx context.Context) int {
	open := 0
	_, err := drain(ctx, func(ctx context.Context) (int, error) {
		n, err := bench.TenantOpen(ctx, rn.db, benchTenant)
		open = n
		return n, err
	}, rn.cfg.drain, time.Second)
	if err != nil {
		logf("settle: %v", err)
	}
	if open > 0 {
		logf("settle: %d actions still open from earlier steps", open)
	}
	return open
}

func (rn *runner) actionStep(ctx context.Context, level int, rate float64) bench.Step {
	st := bench.Step{Offered: rate, NA: map[string]string{}, PreexistingOpen: rn.settle(ctx)}
	requests := int(rate*(rn.cfg.warmup+rn.cfg.measure).Seconds()) + 1
	tr := newTraffic(rn.s.api, rn.gw, rn.s.agents, uint64(level)<<32|uint64(rate), requests,
		fmt.Sprintf("b-%d-%d", len(rn.s.agents), int(rate)), rn.o.erpDelayMS)
	calls0, total0, err0 := bench.ReadFunctionStats(ctx, rn.db, "budget_reserve")
	before, err := rn.dbNow(ctx)
	if err != nil {
		return failed(st, "database", err)
	}
	load, sm, from, to, err := rn.measured(ctx, rate, tr.action)
	if err != nil {
		return failed(st, "window", err)
	}
	st.Requests, st.Errors, st.Throttled = bench.Counts(load.Samples)
	st.ReplayMismatched, st.ReplayMissing = bench.ReplayCheck(load.Samples)
	var admission, replays []time.Duration
	var all, measured []uuid.UUID
	accepted := 0
	for _, s := range load.Samples {
		ok := s.Err == "" && s.Status >= 200 && s.Status <= 202
		if s.Replay {
			if s.Measured && ok {
				replays = append(replays, s.Latency)
			}
			continue
		}
		if s.Measured {
			admission = append(admission, s.Latency)
		}
		id, perr := uuid.Parse(s.ActionID)
		if !ok || perr != nil {
			continue
		}
		all = append(all, id)
		if s.Measured {
			measured = append(measured, id)
			accepted++
		}
	}
	st.Admission, st.Idempotency = bench.Summarize(admission), bench.Summarize(replays)
	st.AdmissionThroughput = float64(accepted) / rn.cfg.measure.Seconds()

	incomplete, err := drain(ctx, func(ctx context.Context) (int, error) {
		return bench.OpenCount(ctx, rn.db, benchTenant, all)
	}, rn.cfg.drain, time.Second)
	if err != nil {
		return failed(st, "drain", err)
	}
	st.Incomplete = load.Incomplete || incomplete
	tls, err := bench.ReadTimelines(ctx, rn.db, benchTenant, all)
	if err != nil {
		return failed(st, "timelines", err)
	}
	inWindow := make(map[string]bool, len(measured))
	for _, id := range measured {
		inWindow[id.String()] = true
	}
	var mtls []bench.Timeline
	for _, tl := range tls {
		if inWindow[tl.ActionID] {
			mtls = append(mtls, tl)
		}
	}
	stages := bench.ComputeStages(mtls)
	st.Governance, st.QueueWait, st.Lease = bench.Summarize(stages.Governance), bench.Summarize(stages.QueueWait),
		bench.Summarize(stages.Lease)
	st.External, st.Overhead, st.EndToEnd = bench.Summarize(stages.External), bench.Summarize(stages.Overhead),
		bench.Summarize(stages.EndToEnd)
	st.Excluded, st.Terminal, st.Open = stages.Excluded, stages.Terminal, stages.Open
	st.CompletedThroughput = float64(bench.CompletedIn(tls, from, to)) / to.Sub(from).Seconds()
	if st.DuplicateKeys, err = bench.DuplicateKeys(ctx, rn.db, benchTenant, before); err != nil {
		st.NA["duplicate_keys"] = firstLine(err.Error())
	}
	calls1, total1, err1 := bench.ReadFunctionStats(ctx, rn.db, "budget_reserve")
	switch {
	case err0 != nil || err1 != nil:
		st.NA["budget_reserve"] = "pg_stat_user_functions unreadable"
	case calls1 > calls0:
		mean := (total1 - total0) / float64(calls1-calls0)
		st.BudgetReserveMeanMs = &mean
	default:
		st.NA["budget_reserve"] = "no budget_reserve calls recorded (track_functions)"
	}
	sm.resources(&st)
	// Replays create no action: completions are compared with new actions.
	st.Saturated = bench.SaturationReasons(bench.Verdict{Throughput: st.CompletedThroughput,
		Offered: bench.NewActionsOffered(load.Samples, rn.cfg.measure), Requests: st.Requests, Errors: st.Errors,
		Throttled: st.Throttled, EndToEndP99Ms: st.EndToEnd.P99Ms})
	logf("%d agents, %v/s: completed %.1f/s, errors %d, 429 %d, e2e %s ms, %s", len(rn.s.agents), rate,
		st.CompletedThroughput, st.Errors, st.Throttled, p50p99(st.EndToEnd), status(st))
	return st
}

func (rn *runner) llmStep(ctx context.Context, rate float64) bench.Step {
	st := bench.Step{Offered: rate, NA: map[string]string{}, PreexistingOpen: rn.settle(ctx)}
	requests := int(rate*(rn.cfg.warmup+rn.cfg.measure).Seconds()) + 1
	tr := newTraffic(rn.s.api, rn.gw, rn.s.agents, 0x4c4c4d<<32|uint64(rate), requests, "", 0)
	load, sm, from, to, err := rn.measured(ctx, rate, tr.llm)
	if err != nil {
		return failed(st, "window", err)
	}
	st.Requests, st.Errors, st.Throttled = bench.Counts(load.Samples)
	var client []time.Duration
	for _, s := range load.Samples {
		if s.Measured && s.Err == "" && s.Status == 200 {
			client = append(client, s.Latency)
		}
	}
	st.LLMClient = bench.Summarize(client)
	// Answers that arrived inside the window, not answers to requests sent in
	// it (which would equal the offered rate by construction).
	st.CompletedThroughput = float64(bench.ArrivedIn(load.Samples, load.Started, load.Ended)) / rn.cfg.measure.Seconds()
	st.AdmissionThroughput = float64(len(client)) / rn.cfg.measure.Seconds()
	st.Incomplete = load.Incomplete
	ledger, err := bench.ReadLLMCalls(ctx, rn.db, benchTenant, from, to)
	if err != nil {
		st.NA["llm_ledger"] = firstLine(err.Error())
	}
	st.LLMLedger = bench.Summarize(ledger)
	sm.resources(&st)
	st.Saturated = bench.SaturationReasons(bench.Verdict{Throughput: st.CompletedThroughput, Offered: rate,
		Requests: st.Requests, Errors: st.Errors, Throttled: st.Throttled, EndToEndP99Ms: st.LLMClient.P99Ms})
	logf("LLM %v/s: %.1f/s, errors %d, 429 %d, client %s ms, %s", rate, st.CompletedThroughput, st.Errors,
		st.Throttled, p50p99(st.LLMClient), status(st))
	return st
}

// failed marks a step whose figures could not be collected.
func failed(st bench.Step, what string, err error) bench.Step {
	st.Incomplete = true
	st.NA[what] = firstLine(err.Error())
	logf("%v/s: %s failed: %v", st.Offered, what, err)
	return st
}

func p50p99(s bench.Summary) string {
	if s.Count == 0 {
		return "n/a"
	}
	return fmt.Sprintf("p50 %.1f p99 %.1f", s.P50Ms, s.P99Ms)
}

func status(st bench.Step) string {
	switch {
	case st.Incomplete:
		return "incomplete"
	case len(st.Saturated) > 0:
		return fmt.Sprintf("saturated %v", st.Saturated)
	}
	return "ok"
}
