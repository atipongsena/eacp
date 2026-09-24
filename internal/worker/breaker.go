package worker

import (
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
)

// maxBreakerCooldown caps a breaker's cooldown. It is also the longest a
// worker may open a connector's shared circuit (migration 00013).
const maxBreakerCooldown = 10 * time.Minute

// jitteredBackoff is the default delay before retry attempt n+1: 1s
// doubling, at most 5m, with equal jitter (a delay d becomes a uniform
// value in [d/2, d]), so actions that failed together do not return
// together (ADR-022 §4).
func jitteredBackoff(n int) time.Duration {
	d := min(time.Second<<min(max(n-1, 0), 9), 5*time.Minute)
	return d/2 + rand.N(d/2+1)
}

// signal is what one call tells a circuit breaker about its connector.
type signal int

const (
	success signal = iota // the target answered
	failure               // the target is unreachable or failing
	neutral               // the worker cancelled the call itself
)

func (s signal) String() string { return [...]string{"success", "failure", "neutral"}[s] }

// signalOf classifies a call's result for the breaker (ADR-022 §3). A
// certified no-effect means the target answered, unless the connection was
// refused before anything was sent.
func signalOf(r Result, cancelledByWorker bool) signal {
	switch {
	case cancelledByWorker:
		return neutral
	case r.Outcome == Succeeded:
		return success
	case r.Outcome == NoEffect && r.ErrorClass != "connection_refused_before_send":
		return success
	default:
		return failure
	}
}

type breakerKey struct{ tenant, connector uuid.UUID }

type breakerState int

const (
	closed breakerState = iota
	open
	halfOpen
)

type circuit struct {
	state     breakerState
	failures  int // consecutive, while closed
	trips     int // consecutive, reset by a successful probe
	openUntil time.Time
	probing   bool
}

// breaker is a worker's circuit breaker per tenant and connector (ADR-022
// §3): CLOSED → OPEN after a run of failures → HALF_OPEN after the cooldown
// → one probe → CLOSED on success or OPEN again, with the cooldown doubled
// up to maxBreakerCooldown.
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu       sync.Mutex
	circuits map[breakerKey]*circuit
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: now, circuits: map[breakerKey]*circuit{}}
}

// get returns k's circuit, moving an open one whose cooldown has passed to
// half-open. The caller holds b.mu.
func (b *breaker) get(k breakerKey) *circuit {
	c := b.circuits[k]
	if c == nil {
		c = &circuit{}
		b.circuits[k] = c
	}
	if c.state == open && !b.now().Before(c.openUntil) {
		c.state, c.probing = halfOpen, false
	}
	return c
}

// blocked reports whether a call to k would be refused now.
func (b *breaker) blocked(k breakerKey) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.get(k)
	return c.state == open || (c.state == halfOpen && c.probing)
}

// blockedKeys lists the connectors whose calls would be refused now.
func (b *breaker) blockedKeys() []breakerKey {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []breakerKey
	for k := range b.circuits {
		if c := b.get(k); c.state == open || (c.state == halfOpen && c.probing) {
			out = append(out, k)
		}
	}
	return out
}

// start admits a call to k: always when closed, once when half-open (the
// probe), never when open. Every admitted call must be followed by done,
// passing on whether it was the probe.
func (b *breaker) start(k breakerKey) (admitted, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.get(k)
	switch {
	case c.state == closed:
		return true, false
	case c.state == halfOpen && !c.probing:
		c.probing = true
		return true, true
	}
	return false, false
}

// done records the signal of a call to k that start admitted. It returns
// the cooldown when this call opened the breaker, and zero otherwise.
func (b *breaker) done(k breakerKey, s signal, probe bool) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.get(k)
	switch c.state {
	case closed:
		if s == success {
			c.failures = 0
		} else if s == failure {
			if c.failures++; c.failures >= b.threshold {
				return b.trip(c)
			}
		}
	case halfOpen:
		switch s {
		case success:
			*c = circuit{}
		case failure:
			return b.trip(c)
		default:
			if probe { // only the probe's own end frees the probe slot
				c.probing = false
			}
		}
	}
	// Open: a call admitted before the trip reports late; it changes nothing.
	return 0
}

func (b *breaker) trip(c *circuit) time.Duration {
	d := min(b.cooldown<<min(c.trips, 10), maxBreakerCooldown)
	c.state, c.failures, c.probing = open, 0, false
	c.trips++
	c.openUntil = b.now().Add(d)
	return d
}
