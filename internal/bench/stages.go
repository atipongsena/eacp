package bench

import (
	"slices"
	"time"
)

// Transition is one journaled state change of an action: From is empty for
// its receipt (action.received). At is the journal's recorded_at, taken with
// clock_timestamp() under the chain head lock.
type Transition struct {
	From, To string
	At       time.Time
}

// Timeline is everything PostgreSQL recorded about one action: its
// transitions in journal order and its first attempt's dispatch and
// completion. It holds database times only.
type Timeline struct {
	ActionID    string
	Transitions []Transition
	Dispatched  *time.Time
	Completed   *time.Time
}

// TerminalStates are the states an action does not leave on its own.
var TerminalStates = []string{"SUCCEEDED", "FAILED", "DENIED", "CANCELLED", "EXPIRED", "NEEDS_HUMAN_RESOLUTION"}

// Stages holds one duration per action for each stage that could be
// computed. Excluded counts the actions a stage had to leave out, Terminal
// the final states reached, and Open the actions that reached none.
type Stages struct {
	Governance, QueueWait, Lease, External, Overhead, EndToEnd []time.Duration
	Excluded                                                   map[string]int
	Terminal                                                   map[string]int
	Open                                                       int
}

// ComputeStages derives the spec §3.4 stages. Retries count their first
// lease and first attempt only.
func ComputeStages(ts []Timeline) Stages {
	s := Stages{Excluded: map[string]int{}, Terminal: map[string]int{}}
	for _, tl := range ts {
		var received, decided, queued, leased, terminal *time.Time
		final := ""
		for _, tr := range tl.Transitions {
			switch {
			case tr.From == "" && received == nil:
				received = ptrTo(tr.At)
			case tr.From == "RECEIVED" && decided == nil:
				decided = ptrTo(tr.At)
			}
			if tr.To == "QUEUED" && queued == nil {
				queued = ptrTo(tr.At)
			}
			if tr.To == "LEASED" && leased == nil {
				leased = ptrTo(tr.At)
			}
			if slices.Contains(TerminalStates, tr.To) && terminal == nil {
				terminal = ptrTo(tr.At)
				final = tr.To
			}
		}
		governance, okG := span(received, decided)
		external, okX := span(tl.Dispatched, tl.Completed)
		endToEnd, okE := span(received, terminal)
		s.add("governance", &s.Governance, governance, okG)
		queueWait, ok := span(queued, leased)
		s.add("queue_wait", &s.QueueWait, queueWait, ok)
		lease, ok := span(leased, tl.Dispatched)
		s.add("lease", &s.Lease, lease, ok)
		s.add("external", &s.External, external, okX)
		s.add("end_to_end", &s.EndToEnd, endToEnd, okE)
		s.add("overhead", &s.Overhead, endToEnd-governance-external, okG && okX && okE)
		if final == "" {
			s.Open++
		} else {
			s.Terminal[final]++
		}
	}
	return s
}

func (s *Stages) add(name string, into *[]time.Duration, d time.Duration, ok bool) {
	if ok {
		*into = append(*into, d)
	} else {
		s.Excluded[name]++
	}
}

func span(from, to *time.Time) (time.Duration, bool) {
	if from == nil || to == nil {
		return 0, false
	}
	return to.Sub(*from), true
}

func ptrTo(t time.Time) *time.Time { return &t }

// CompletedIn counts the actions whose first terminal transition happened
// in [from, to), both in database time: completions inside a measurement
// window, not the backlog drained after it.
func CompletedIn(ts []Timeline, from, to time.Time) int {
	n := 0
	for _, tl := range ts {
		for _, tr := range tl.Transitions {
			if slices.Contains(TerminalStates, tr.To) {
				if !tr.At.Before(from) && tr.At.Before(to) {
					n++
				}
				break
			}
		}
	}
	return n
}
