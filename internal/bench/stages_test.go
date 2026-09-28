package bench

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func at(msec int) time.Time { return t0.Add(time.Duration(msec) * time.Millisecond) }

func ptr(t time.Time) *time.Time { return &t }

func only(t *testing.T, name string, got []time.Duration, want int) {
	t.Helper()
	if !slices.Equal(got, ms(want)) {
		t.Fatalf("%s = %v, want [%dms]", name, got, want)
	}
}

func TestHappyPathStages(t *testing.T) {
	s := ComputeStages([]Timeline{{ActionID: "a", Transitions: []Transition{
		{To: "RECEIVED", At: at(0)},
		{From: "RECEIVED", To: "QUEUED", At: at(10)},
		{From: "QUEUED", To: "LEASED", At: at(30)},
		{From: "LEASED", To: "EXECUTING", At: at(31)},
		{From: "EXECUTING", To: "SUCCEEDED", At: at(45)},
	}, Dispatched: ptr(at(32)), Completed: ptr(at(40))}})
	only(t, "governance", s.Governance, 10)
	only(t, "queue wait", s.QueueWait, 20)
	only(t, "lease", s.Lease, 2)
	only(t, "external", s.External, 8)
	only(t, "end-to-end", s.EndToEnd, 45)
	only(t, "overhead", s.Overhead, 27)
	if s.Terminal["SUCCEEDED"] != 1 || s.Open != 0 || len(s.Excluded) != 0 {
		t.Fatalf("terminal %v, open %d, excluded %v", s.Terminal, s.Open, s.Excluded)
	}
}

func TestARetryCountsTheFirstAttempt(t *testing.T) {
	s := ComputeStages([]Timeline{{ActionID: "a", Transitions: []Transition{
		{To: "RECEIVED", At: at(0)},
		{From: "RECEIVED", To: "AUTHORIZED", At: at(4)},
		{From: "AUTHORIZED", To: "QUEUED", At: at(5)},
		{From: "QUEUED", To: "LEASED", At: at(15)},
		{From: "LEASED", To: "EXECUTING", At: at(16)},
		{From: "EXECUTING", To: "RETRY_WAIT", At: at(20)},
		{From: "RETRY_WAIT", To: "QUEUED", At: at(120)},
		{From: "QUEUED", To: "LEASED", At: at(200)},
		{From: "LEASED", To: "EXECUTING", At: at(201)},
		{From: "EXECUTING", To: "SUCCEEDED", At: at(210)},
	}, Dispatched: ptr(at(17)), Completed: ptr(at(19))}})
	only(t, "governance", s.Governance, 4)
	only(t, "queue wait", s.QueueWait, 10)
	only(t, "lease", s.Lease, 2)
	only(t, "end-to-end", s.EndToEnd, 210)
}

func TestADeniedActionHasOnlyGovernanceAndEndToEnd(t *testing.T) {
	s := ComputeStages([]Timeline{{ActionID: "a", Transitions: []Transition{
		{To: "RECEIVED", At: at(0)},
		{From: "RECEIVED", To: "DENIED", At: at(5)},
	}}})
	only(t, "governance", s.Governance, 5)
	only(t, "end-to-end", s.EndToEnd, 5)
	want := map[string]int{"queue_wait": 1, "lease": 1, "external": 1, "overhead": 1}
	if !reflect.DeepEqual(s.Excluded, want) || s.Terminal["DENIED"] != 1 {
		t.Fatalf("excluded %v, terminal %v; want %v and one DENIED", s.Excluded, s.Terminal, want)
	}
}

func TestAnUnknownOutcomeIsOpen(t *testing.T) {
	s := ComputeStages([]Timeline{{ActionID: "a", Transitions: []Transition{
		{To: "RECEIVED", At: at(0)},
		{From: "RECEIVED", To: "QUEUED", At: at(3)},
		{From: "QUEUED", To: "LEASED", At: at(4)},
		{From: "LEASED", To: "EXECUTING", At: at(5)},
		{From: "EXECUTING", To: "UNKNOWN_OUTCOME", At: at(3000)},
	}, Dispatched: ptr(at(6))}})
	if s.Open != 1 || len(s.Terminal) != 0 {
		t.Fatalf("open %d, terminal %v; want one open action", s.Open, s.Terminal)
	}
	if s.Excluded["end_to_end"] != 1 || s.Excluded["external"] != 1 || len(s.EndToEnd) != 0 {
		t.Fatalf("excluded %v, end-to-end %v", s.Excluded, s.EndToEnd)
	}
}

// Every Timeline time comes from PostgreSQL; the client's clock never enters
// a stage, so the two clocks are never subtracted from each other.
func TestStagesUseOnlyDatabaseTimes(t *testing.T) {
	_ = Timeline{ActionID: "a", Transitions: []Transition{{From: "RECEIVED", To: "QUEUED", At: at(1)}},
		Dispatched: ptr(at(2)), Completed: ptr(at(3))}
	if n := reflect.TypeOf(Timeline{}).NumField(); n != 4 {
		t.Fatalf("Timeline has %d fields, want 4: a stage must not read a client time", n)
	}
}
