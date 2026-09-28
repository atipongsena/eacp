package bench

import (
	"slices"
	"testing"
	"time"
)

func ms(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func TestSummarizeNearestRank(t *testing.T) {
	var hundred []time.Duration
	for i := 100; i >= 1; i-- {
		hundred = append(hundred, time.Duration(i)*time.Millisecond)
	}
	before := slices.Clone(hundred)
	if got, want := Summarize(hundred), (Summary{Count: 100, P50Ms: 50, P95Ms: 95, P99Ms: 99, MaxMs: 100}); got != want {
		t.Fatalf("1..100 ms = %+v, want %+v", got, want)
	}
	if !slices.Equal(hundred, before) {
		t.Fatal("Summarize reordered its input")
	}
	if got, want := Summarize(ms(7)), (Summary{Count: 1, P50Ms: 7, P95Ms: 7, P99Ms: 7, MaxMs: 7}); got != want {
		t.Fatalf("one sample = %+v, want %+v", got, want)
	}
	if got := Summarize(ms(1, 1, 1, 9)); got.P50Ms != 1 || got.P99Ms != 9 {
		t.Fatalf("ties = %+v, want P50 1 and P99 9", got)
	}
	if got := Summarize(nil); got != (Summary{}) {
		t.Fatalf("empty = %+v, want the zero Summary", got)
	}
}
