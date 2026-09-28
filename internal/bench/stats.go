// Package bench is the load benchmark's logic (MASTER_PLAN §104, §105): the
// open-loop scheduler, percentiles, per-stage timings from the audit journal,
// the saturation rule, resource samples, the results file and its report.
// cmd/eacp-bench drives a live stack with it.
package bench

import (
	"math"
	"slices"
	"time"
)

// Summary is a latency distribution in milliseconds. Count 0 means there
// were no samples: it is rendered as n/a, never as zero.
type Summary struct {
	Count int     `json:"count"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

// Summarize computes exact nearest-rank percentiles (rank = ⌈p/100 · n⌉) of
// samples without reordering them.
func Summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	at := func(p float64) float64 {
		rank := int(math.Ceil(p / 100 * float64(len(s))))
		return toMs(s[max(rank, 1)-1])
	}
	return Summary{Count: len(s), P50Ms: at(50), P95Ms: at(95), P99Ms: at(99), MaxMs: toMs(s[len(s)-1])}
}

func toMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
