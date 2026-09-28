package bench

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Clock is the scheduler's time source; tests use virtual time.
type Clock interface {
	Now() time.Time
	SleepUntil(ctx context.Context, t time.Time) error
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) SleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Plan is one open-loop step: Rate requests per second for Warmup (not
// measured) and then Measure. MaxInFlight bounds unanswered requests; 0
// means no bound.
type Plan struct {
	Rate        float64
	Warmup      time.Duration
	Measure     time.Duration
	MaxInFlight int
}

// Response is what a sender reports for one request.
type Response struct {
	Status   int
	Err      error
	ActionID string
}

// Sample is one request. Latency counts from its intended send time, so a
// slow server shows as latency, never as a slower sender.
type Sample struct {
	Index    int
	Intended time.Time
	Latency  time.Duration
	Status   int
	Err      string
	Measured bool
	Replay   bool
	ActionID string
}

// Load is the outcome of one step. Started and Ended bound the measurement
// window. Incomplete means the step was cut short (the in-flight bound or a
// cancelled context), so its figures do not describe the offered rate.
type Load struct {
	Samples    []Sample
	Incomplete bool
	Started    time.Time
	Ended      time.Time
}

// RunOpenLoop sends request i at t0 + i/Rate, each in its own goroutine,
// without waiting for earlier answers, and waits for every answer. When the
// in-flight bound would be exceeded it stops sending, cancels what is in
// flight and marks the load incomplete.
func RunOpenLoop(ctx context.Context, clk Clock, p Plan, send func(ctx context.Context, i int) Response) Load {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t0 := clk.Now()
	load := Load{Started: t0.Add(p.Warmup)}
	load.Ended = load.Started.Add(p.Measure)

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		inFlight int
	)
	for i := 0; ; i++ {
		intended := t0.Add(time.Duration(float64(i) * float64(time.Second) / p.Rate))
		if !intended.Before(load.Ended) {
			break
		}
		if err := clk.SleepUntil(ctx, intended); err != nil {
			load.Incomplete = true
			break
		}
		mu.Lock()
		if p.MaxInFlight > 0 && inFlight >= p.MaxInFlight {
			mu.Unlock()
			load.Incomplete = true
			cancel()
			break
		}
		inFlight++
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := send(ctx, i)
			s := Sample{Index: i, Intended: intended, Latency: clk.Now().Sub(intended), Status: r.Status,
				Measured: !intended.Before(load.Started), Replay: IsReplay(i), ActionID: r.ActionID}
			if r.Err != nil {
				s.Err = r.Err.Error()
			}
			mu.Lock()
			inFlight--
			load.Samples = append(load.Samples, s)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(load.Samples, func(a, b Sample) int { return a.Index - b.Index })
	return load
}

// IsReplay reports whether request i replays an earlier request's
// idempotency key (5% of requests).
func IsReplay(i int) bool { return i%20 == 19 }

// ReplayOf is the request that replay i repeats.
func ReplayOf(i int) int { return i - 10 }

// Counts tallies measured samples: 429 is throttled; any other non-2xx
// status or a transport error is an error.
func Counts(samples []Sample) (requests, errors, throttled int) {
	for _, s := range samples {
		if !s.Measured {
			continue
		}
		requests++
		switch {
		case s.Err == "" && s.Status == 429:
			throttled++
		case s.Err != "" || s.Status < 200 || s.Status > 299:
			errors++
		}
	}
	return requests, errors, throttled
}
