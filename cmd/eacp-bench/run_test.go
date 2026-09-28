package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"eacp/internal/bench"
)

func TestResolveQuickAndFull(t *testing.T) {
	full := resolve(false)
	if !slices.Equal(full.levels, []int{100, 1000, 5000, 10000}) ||
		!slices.Equal(full.actionRates, []float64{25, 50, 100, 200, 400, 800}) ||
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
