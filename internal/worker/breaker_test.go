package worker

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBreakerOpensProbesAndCloses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	b := newBreaker(3, 10*time.Second, func() time.Time { return now })
	k := breakerKey{uuid.New(), uuid.New()}
	other := breakerKey{k.tenant, uuid.New()}

	// Two failures and a success keep it closed; three in a row open it.
	for _, s := range []signal{failure, failure, success, failure, failure} {
		if ok, _ := b.start(k); !ok {
			t.Fatal("a closed breaker refused a call")
		}
		if d := b.done(k, s, false); d != 0 {
			t.Fatalf("tripped early after %v", s)
		}
	}
	b.start(k)
	if d := b.done(k, failure, false); d != 10*time.Second {
		t.Fatalf("trip cooldown = %v, want 10s", d)
	}
	if ok, _ := b.start(k); !b.blocked(k) || ok {
		t.Fatal("an open breaker let a call through")
	}
	if ok, _ := b.start(other); b.blocked(other) || !ok {
		t.Fatal("another connector's breaker is affected")
	}
	b.done(other, success, false)

	// After the cooldown one probe goes through; a second waits for it.
	now = now.Add(10 * time.Second)
	if b.blocked(k) {
		t.Fatal("half-open breaker blocks its probe")
	}
	if ok, probe := b.start(k); !ok || !probe {
		t.Fatal("half-open breaker refused its probe")
	}
	if ok, _ := b.start(k); !b.blocked(k) || ok {
		t.Fatal("half-open breaker allowed a second concurrent probe")
	}
	// A call admitted before the trip ending neutral does not free the slot.
	if d := b.done(k, neutral, false); d != 0 || !b.blocked(k) {
		t.Fatal("a non-probe call freed the probe slot")
	}
	// A failed probe re-opens it with a doubled cooldown.
	if d := b.done(k, failure, true); d != 20*time.Second {
		t.Fatalf("second trip cooldown = %v, want 20s", d)
	}
	now = now.Add(20 * time.Second)
	// A neutral probe (the worker cancelled it) frees the probe slot only.
	_, probe := b.start(k)
	if d := b.done(k, neutral, probe); d != 0 || b.blocked(k) {
		t.Fatalf("neutral probe changed the state (trip %v, blocked %v)", d, b.blocked(k))
	}
	// A successful probe closes it and resets the escalation.
	_, probe = b.start(k)
	if d := b.done(k, success, probe); d != 0 || b.blocked(k) {
		t.Fatal("successful probe did not close the breaker")
	}
	for range 2 {
		b.start(k)
		b.done(k, failure, false)
	}
	b.start(k)
	if d := b.done(k, failure, false); d != 10*time.Second {
		t.Fatalf("cooldown after recovery = %v, want the base 10s", d)
	}
	// Results of calls started before the trip do not trip it again.
	if d := b.done(k, failure, false); d != 0 {
		t.Fatalf("a late failure re-tripped an open breaker (%v)", d)
	}
}

func TestBreakerCooldownIsCapped(t *testing.T) {
	now := time.Unix(0, 0)
	b := newBreaker(1, 4*time.Minute, func() time.Time { return now })
	k := breakerKey{uuid.New(), uuid.New()}
	var last time.Duration
	for range 5 {
		ok, probe := b.start(k)
		if !ok {
			t.Fatal("expected a probe")
		}
		last = b.done(k, failure, probe)
		now = now.Add(last)
	}
	if last != maxBreakerCooldown {
		t.Fatalf("cooldown = %v, want the %v cap", last, maxBreakerCooldown)
	}
}

func TestDefaultBackoffIsJitteredWithinBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for n := 1; n <= 12; n++ {
		full := min(time.Second<<min(n-1, 9), 5*time.Minute)
		for range 50 {
			d := jitteredBackoff(n)
			if d < full/2 || d > full {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v]", n, d, full/2, full)
			}
			seen[d] = true
		}
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct delays: not jittered", len(seen))
	}
}

func TestBreakerSignals(t *testing.T) {
	cases := []struct {
		r         Result
		cancelled bool
		want      signal
	}{
		{Result{Outcome: Succeeded, ExternalReference: "x"}, false, success},
		{Result{Outcome: NoEffect, ErrorClass: "validation"}, false, success},
		{Result{Outcome: NoEffect, ErrorClass: "connection_refused_before_send"}, false, failure},
		{Result{Outcome: Ambiguous, ErrorClass: "timeout"}, false, failure},
		{Result{Outcome: Ambiguous, ErrorClass: "transport_error"}, true, neutral},
	}
	for _, c := range cases {
		if got := signalOf(c.r, c.cancelled); got != c.want {
			t.Errorf("signalOf(%+v, %v) = %v, want %v", c.r, c.cancelled, got, c.want)
		}
	}
}
