package bench

import "time"

// The saturation thresholds (spec §3.3).
const (
	ThroughputFloor = 0.95
	ErrorCeiling    = 0.01
	P99Ceiling      = 5 * time.Second
)

// Verdict is what the saturation rule reads from one measured step.
type Verdict struct {
	Throughput    float64 // actions that reached a terminal state per second
	Offered       float64 // the step's rate
	Requests      int
	Errors        int
	Throttled     int // HTTP 429
	EndToEndP99Ms float64
}

// SaturationReasons returns why a step is saturated ("throughput", "errors",
// "p99", in that order), or nil when it is sustainable.
func SaturationReasons(v Verdict) []string {
	var why []string
	if v.Throughput < ThroughputFloor*v.Offered {
		why = append(why, "throughput")
	}
	if float64(v.Errors+v.Throttled) > ErrorCeiling*float64(v.Requests) {
		why = append(why, "errors")
	}
	if v.EndToEndP99Ms > toMs(P99Ceiling) {
		why = append(why, "p99")
	}
	return why
}
