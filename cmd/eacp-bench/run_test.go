package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atipongsena/eacp/internal/bench"
)

func TestResolveQuickAndFull(t *testing.T) {
	full := resolve(false)
	if !slices.Equal(full.levels, []int{100, 1000, 5000, 10000}) ||
		!slices.Equal(full.actionRates, []float64{25, 30, 35, 40, 45, 50, 100, 200, 400, 800}) ||
		!slices.Equal(full.llmRates, []float64{25, 50, 100, 200}) ||
		full.warmup != 10*time.Second || full.measure != 60*time.Second || full.drain != 5*time.Minute ||
		full.maxInFlight != 20000 {
		t.Fatalf("full = %+v", full)
	}
	quick := resolve(true)
	if !slices.Equal(quick.levels, []int{100}) || !slices.Equal(quick.actionRates, []float64{25, 50}) ||
		!slices.Equal(quick.llmRates, []float64{25}) || quick.warmup != 5*time.Second || quick.measure != 20*time.Second {
		t.Fatalf("quick = %+v", quick)
	}
}

func TestDrainTimesOut(t *testing.T) {
	stuck := func(context.Context) (int, error) { return 3, nil }
	incomplete, err := drain(context.Background(), stuck, 50*time.Millisecond, 10*time.Millisecond)
	if err != nil || !incomplete {
		t.Fatalf("a backlog that never drains = %v %v, want incomplete", incomplete, err)
	}
	calls := 0
	draining := func(context.Context) (int, error) {
		calls++
		if calls >= 3 {
			return 0, nil
		}
		return 2, nil
	}
	incomplete, err = drain(context.Background(), draining, time.Second, 10*time.Millisecond)
	if err != nil || incomplete || calls != 3 {
		t.Fatalf("a draining backlog = %v %v after %d checks", incomplete, err, calls)
	}
}

func TestClimbStopsAtTheFirstBadStep(t *testing.T) {
	var ran []float64
	step := func(rate float64) bench.Step {
		ran = append(ran, rate)
		s := bench.Step{Offered: rate}
		if rate >= 100 {
			s.Saturated = []string{"throughput"}
		}
		return s
	}
	steps, best := climb([]float64{25, 50, 100, 200}, step)
	if !slices.Equal(ran, []float64{25, 50, 100}) || len(steps) != 3 || best != 50 {
		t.Fatalf("ran %v, %d steps, best %v; want [25 50 100], 3 and 50", ran, len(steps), best)
	}
	_, best = climb([]float64{25, 50}, func(r float64) bench.Step { return bench.Step{Offered: r, Incomplete: true} })
	if best != 0 {
		t.Fatalf("every step incomplete gave best %v, want 0", best)
	}
}

// The overrides printed in every report are the ones the bench compose
// files actually set.
func TestOverridesMatchTheComposeFiles(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deployments", "bench", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	bench, local := read("compose.bench.yml"), read("compose.bench-local.yml")
	for _, pdp := range []string{"local", "microsoft-agt"} {
		for _, o := range overrides(pdp) {
			file := bench
			if o.Name == "EACP_GOVERNANCE_PROVIDER" {
				file = local
			}
			want := fmt.Sprintf("%s: %q", o.Name, o.Value)
			if o.Name == "track_functions" {
				want = `"track_functions=pl"`
			}
			if !strings.Contains(file, want) {
				t.Errorf("%s: the compose files do not set %s", pdp, want)
			}
		}
	}
}
