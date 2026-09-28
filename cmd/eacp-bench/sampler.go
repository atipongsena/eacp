package main

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"runtime/metrics"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/atipongsena/eacp/internal/bench"
)

const sampleEvery = 2 * time.Second

// sampler reads docker stats, PostgreSQL and NATS every two seconds while a
// step is measured. A source that fails keeps its first error as the reason
// it is reported as n/a.
type sampler struct {
	s  *setup
	db *pgxpool.Pool

	mu         sync.Mutex
	containers []bench.ContainerSample
	dbs        []bench.DBSample
	nats       []bench.NATSSample
	self       []bench.ContainerSample
	failures   map[string]string
}

func (sm *sampler) fail(source string, err error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, ok := sm.failures[source]; !ok {
		sm.failures[source] = firstLine(err.Error())
	}
}

// run samples until ctx ends; each source has its own loop so a slow
// `docker stats` does not delay the others.
func (sm *sampler) run(ctx context.Context) {
	sm.failures = map[string]string{}
	var wg sync.WaitGroup
	loop := func(fn func(context.Context)) {
		wg.Go(func() {
			for {
				next := time.Now().Add(sampleEvery)
				fn(ctx)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(next)):
				}
			}
		})
	}
	loop(sm.docker)
	loop(sm.database)
	loop(sm.natsz)
	wg.Go(func() { sm.ownCPU(ctx) })
	wg.Wait()
}

func (sm *sampler) docker(ctx context.Context) {
	out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "json").Output()
	if err != nil {
		if ctx.Err() == nil {
			sm.fail("docker", err)
		}
		return
	}
	var got []bench.ContainerSample
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if !strings.Contains(sc.Text(), `"`+sm.s.o.project+`-`) {
			continue // another project's container
		}
		c, err := bench.ParseDockerStats(sc.Bytes(), sm.s.o.project)
		if err != nil {
			continue // a container between states; the others still count
		}
		got = append(got, c)
	}
	sm.mu.Lock()
	sm.containers = append(sm.containers, got...)
	sm.mu.Unlock()
}

func (sm *sampler) database(ctx context.Context) {
	s, err := bench.ReadDB(ctx, sm.db)
	if err != nil {
		if ctx.Err() == nil {
			sm.fail("db", err)
		}
		return
	}
	sm.mu.Lock()
	sm.dbs = append(sm.dbs, s)
	sm.mu.Unlock()
}

func (sm *sampler) natsz(ctx context.Context) {
	out, err := sm.s.compose(ctx, "exec", "-T", "nats", "wget", "-qO-", "http://127.0.0.1:8222/varz")
	if err != nil {
		if ctx.Err() == nil {
			sm.fail("nats", err)
		}
		return
	}
	n, err := bench.ParseVarz(out, time.Now())
	if err != nil {
		sm.fail("nats", err)
		return
	}
	sm.mu.Lock()
	sm.nats = append(sm.nats, n)
	sm.mu.Unlock()
}

// ownCPU records this process's CPU (Go's user-CPU estimate) and memory as
// the pseudo-container eacp-bench: if the load generator saturates, its
// figures show it.
func (sm *sampler) ownCPU(ctx context.Context) {
	read := func() (float64, int64) {
		s := []metrics.Sample{{Name: "/cpu/classes/user:cpu-seconds"}, {Name: "/memory/classes/total:bytes"}}
		metrics.Read(s)
		return s[0].Value.Float64(), int64(s[1].Value.Uint64())
	}
	lastCPU, _ := read()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sampleEvery):
		}
		cpu, mem := read()
		now := time.Now()
		sm.mu.Lock()
		sm.self = append(sm.self, bench.ContainerSample{Service: "eacp-bench",
			CPUPercent: (cpu - lastCPU) / now.Sub(last).Seconds() * 100, MemBytes: mem})
		sm.mu.Unlock()
		lastCPU, last = cpu, now
	}
}

// resources summarises what was sampled into a step.
func (sm *sampler) resources(st *bench.Step) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	var dockerFail []string
	if f, ok := sm.failures["docker"]; ok {
		dockerFail = []string{"docker: " + f}
	}
	st.Containers = bench.AggregateContainers(sm.containers, dockerFail)
	if len(sm.self) > 0 {
		st.Containers["eacp-bench"] = bench.AggregateContainers(sm.self, nil)["eacp-bench"]
	} else {
		st.Containers["eacp-bench"] = bench.Resource{NA: "no samples"}
	}
	st.DB = bench.DBRates(sm.dbs)
	if f, ok := sm.failures["db"]; ok && st.DB.NA != "" {
		st.DB.NA = "database: " + f
	}
	st.NATS = bench.NATSRates(sm.nats)
	if f, ok := sm.failures["nats"]; ok && st.NATS.NA != "" {
		st.NATS.NA = "nats: " + f
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
