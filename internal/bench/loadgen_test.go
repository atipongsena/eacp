package bench

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is virtual time: SleepUntil jumps forward, and a fake server
// moves time on by its service time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) SleepUntil(ctx context.Context, t time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advanceTo(t)
	return nil
}

func (c *fakeClock) advanceTo(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

// serialServer answers requests one at a time in index order, each taking
// service(i) of virtual time from when it could start.
type serialServer struct {
	clk     *fakeClock
	t0      time.Time
	rate    float64
	service func(i int) time.Duration
	status  func(i int) Response

	mu   sync.Mutex
	cond *sync.Cond
	next int
	busy time.Time
}

func (s *serialServer) send(_ context.Context, i int) Response {
	s.mu.Lock()
	if s.cond == nil {
		s.cond = sync.NewCond(&s.mu)
	}
	for s.next != i {
		s.cond.Wait()
	}
	intended := s.t0.Add(time.Duration(float64(i) * float64(time.Second) / s.rate))
	start := intended
	if s.busy.After(start) {
		start = s.busy
	}
	s.busy = start.Add(s.service(i))
	s.clk.advanceTo(s.busy)
	s.next++
	s.cond.Broadcast()
	s.mu.Unlock()
	if s.status != nil {
		return s.status(i)
	}
	return Response{Status: 201}
}

func TestLatencyIsMeasuredFromTheIntendedTime(t *testing.T) {
	clk := newFakeClock()
	srv := &serialServer{clk: clk, t0: clk.Now(), rate: 10, service: func(i int) time.Duration {
		if i == 0 {
			return 500 * time.Millisecond
		}
		return 0
	}}
	load := RunOpenLoop(context.Background(), clk, Plan{Rate: 10, Measure: time.Second, MaxInFlight: 100}, srv.send)
	if len(load.Samples) != 10 {
		t.Fatalf("%d samples, want 10", len(load.Samples))
	}
	if got := load.Samples[1].Latency; got < 400*time.Millisecond {
		t.Fatalf("request 1 waited behind a 500 ms request but measured %v: latency must count from its intended time", got)
	}
}

func TestWarmupIsExcluded(t *testing.T) {
	clk := newFakeClock()
	srv := &serialServer{clk: clk, t0: clk.Now(), rate: 10, service: func(int) time.Duration { return time.Millisecond }}
	load := RunOpenLoop(context.Background(), clk, Plan{Rate: 10, Warmup: time.Second, Measure: time.Second, MaxInFlight: 100}, srv.send)
	measured := 0
	for _, s := range load.Samples {
		if s.Measured {
			measured++
		}
	}
	if len(load.Samples) != 20 || measured != 10 {
		t.Fatalf("%d samples with %d measured, want 20 and 10", len(load.Samples), measured)
	}
	if load.Incomplete || !load.Ended.Equal(load.Started.Add(time.Second)) {
		t.Fatalf("load = incomplete %v, window %v..%v", load.Incomplete, load.Started, load.Ended)
	}
}

func TestTheInFlightCapMarksIncomplete(t *testing.T) {
	clk := newFakeClock()
	hang := func(ctx context.Context, _ int) Response {
		<-ctx.Done()
		return Response{Err: ctx.Err()}
	}
	load := RunOpenLoop(context.Background(), clk, Plan{Rate: 10, Measure: time.Second, MaxInFlight: 3}, hang)
	if !load.Incomplete {
		t.Fatal("a load over its in-flight cap must be incomplete")
	}
	if len(load.Samples) != 3 {
		t.Fatalf("%d samples, want the 3 in flight", len(load.Samples))
	}
}

func TestErrorsAndThrottlesAreKept(t *testing.T) {
	clk := newFakeClock()
	statuses := []Response{{Status: 201}, {Status: 429}, {Status: 500}, {Err: errors.New("connection reset")}}
	srv := &serialServer{clk: clk, t0: clk.Now(), rate: 4, service: func(int) time.Duration { return time.Millisecond },
		status: func(i int) Response { return statuses[i] }}
	load := RunOpenLoop(context.Background(), clk, Plan{Rate: 4, Measure: time.Second, MaxInFlight: 100}, srv.send)
	if len(load.Samples) != 4 {
		t.Fatalf("%d samples, want 4: no request may be dropped", len(load.Samples))
	}
	if requests, errs, throttled := Counts(load.Samples); requests != 4 || errs != 2 || throttled != 1 {
		t.Fatalf("Counts = %d, %d, %d; want 4, 2, 1", requests, errs, throttled)
	}
	for _, s := range load.Samples {
		if s.Latency <= 0 {
			t.Fatalf("sample %d has latency %v", s.Index, s.Latency)
		}
	}
	if load.Samples[3].Err == "" {
		t.Fatal("a transport error must be kept on its sample")
	}
}

func TestReplaySelection(t *testing.T) {
	var replays []int
	for i := range 60 {
		if IsReplay(i) {
			replays = append(replays, i)
		}
	}
	if len(replays) != 3 || replays[0] != 19 || replays[1] != 39 || replays[2] != 59 {
		t.Fatalf("replays = %v, want [19 39 59]", replays)
	}
	if ReplayOf(19) != 9 {
		t.Fatalf("ReplayOf(19) = %d, want 9", ReplayOf(19))
	}
}

// Replays create no action, so the throughput rule compares completions
// with the new actions offered, not with every request.
func TestNewActionsOfferedExcludeReplays(t *testing.T) {
	var samples []Sample
	for i := range 40 {
		samples = append(samples, Sample{Index: i, Measured: i >= 20, Replay: IsReplay(i)})
	}
	if got := NewActionsOffered(samples, 2*time.Second); got != 9.5 {
		t.Fatalf("NewActionsOffered = %v, want 9.5 (19 new actions over 2 s)", got)
	}
}
