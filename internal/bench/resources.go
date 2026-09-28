package bench

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// KnownServices are the stack's containers whose resources are reported.
var KnownServices = []string{"controlplane-api", "execution-worker", "agt-pdp", "postgres", "nats", "fakeerp",
	"llm-gateway", "fakellm"}

// ContainerSample is one docker stats reading of one service.
type ContainerSample struct {
	Service    string
	CPUPercent float64
	MemBytes   int64
}

// ParseDockerStats parses one line of `docker stats --no-stream --format
// json` for a container of compose project project.
func ParseDockerStats(line []byte, project string) (ContainerSample, error) {
	var raw struct{ Name, CPUPerc, MemUsage string }
	if err := json.Unmarshal(line, &raw); err != nil {
		return ContainerSample{}, fmt.Errorf("docker stats line: %w", err)
	}
	m := regexp.MustCompile(`^` + regexp.QuoteMeta(project) + `-(.+)-\d+$`).FindStringSubmatch(raw.Name)
	if m == nil {
		return ContainerSample{}, fmt.Errorf("container %q is not in project %s", raw.Name, project)
	}
	cpu, err := strconv.ParseFloat(strings.TrimSuffix(raw.CPUPerc, "%"), 64)
	if err != nil {
		return ContainerSample{}, fmt.Errorf("%s CPU %q: %w", raw.Name, raw.CPUPerc, err)
	}
	mem, err := parseBytes(strings.TrimSpace(strings.SplitN(raw.MemUsage, "/", 2)[0]))
	if err != nil {
		return ContainerSample{}, fmt.Errorf("%s memory %q: %w", raw.Name, raw.MemUsage, err)
	}
	return ContainerSample{Service: m[1], CPUPercent: cpu, MemBytes: mem}, nil
}

var byteUnits = []struct {
	suffix string
	scale  float64
}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"kB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}

func parseBytes(s string) (int64, error) {
	for _, u := range byteUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return 0, err
			}
			return int64(v * u.scale), nil
		}
	}
	return 0, fmt.Errorf("no unit in %q", s)
}

// NATSSample is one reading of the NATS server's /varz counters.
type NATSSample struct {
	At                                 time.Time
	InMsgs, OutMsgs, InBytes, OutBytes int64
}

// ParseVarz parses a /varz body read at at.
func ParseVarz(body []byte, at time.Time) (NATSSample, error) {
	var v struct {
		InMsgs   int64 `json:"in_msgs"`
		OutMsgs  int64 `json:"out_msgs"`
		InBytes  int64 `json:"in_bytes"`
		OutBytes int64 `json:"out_bytes"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return NATSSample{}, fmt.Errorf("varz: %w", err)
	}
	return NATSSample{At: at, InMsgs: v.InMsgs, OutMsgs: v.OutMsgs, InBytes: v.InBytes, OutBytes: v.OutBytes}, nil
}

// Resource is one service's CPU and memory over a step. A non-empty NA is
// why it could not be measured; it is then rendered as n/a, never as zero.
type Resource struct {
	MeanCPU     float64 `json:"mean_cpu_percent"`
	MaxCPU      float64 `json:"max_cpu_percent"`
	MaxMemBytes int64   `json:"max_mem_bytes"`
	NA          string  `json:"na,omitempty"`
}

// AggregateContainers summarises samples per service. With no samples at
// all, every known service is n/a with the first failure.
func AggregateContainers(samples []ContainerSample, failures []string) map[string]Resource {
	na := "no samples"
	if len(samples) == 0 && len(failures) > 0 {
		na = failures[0]
	}
	out := map[string]Resource{}
	for _, s := range KnownServices {
		out[s] = Resource{NA: na}
	}
	sums, counts := map[string]float64{}, map[string]int{}
	for _, s := range samples {
		r := out[s.Service]
		if counts[s.Service] == 0 {
			r = Resource{}
		}
		sums[s.Service] += s.CPUPercent
		counts[s.Service]++
		r.MaxCPU = max(r.MaxCPU, s.CPUPercent)
		r.MaxMemBytes = max(r.MaxMemBytes, s.MemBytes)
		r.MeanCPU = sums[s.Service] / float64(counts[s.Service])
		out[s.Service] = r
	}
	return out
}

// Rates are per-second rates and maxima over a step; a non-empty NA is why
// they could not be measured.
type Rates struct {
	PerSecond map[string]float64 `json:"values,omitempty"`
	NA        string             `json:"na,omitempty"`
}

// DBRates computes commits per second and the cache hit ratio between the
// first and last sample, and the highest active and lock-waiting sessions.
func DBRates(samples []DBSample) Rates {
	if len(samples) < 2 {
		return Rates{NA: "fewer than two database samples"}
	}
	first, last := samples[0], samples[len(samples)-1]
	secs := last.At.Sub(first.At).Seconds()
	if secs <= 0 {
		return Rates{NA: "database samples share one time"}
	}
	r := Rates{PerSecond: map[string]float64{"commits_per_s": float64(last.Commits-first.Commits) / secs}}
	hit, read := last.BlksHit-first.BlksHit, last.BlksRead-first.BlksRead
	if hit+read > 0 {
		r.PerSecond["cache_hit_ratio"] = float64(hit) / float64(hit+read)
	}
	for _, s := range samples {
		r.PerSecond["max_active"] = max(r.PerSecond["max_active"], float64(s.Active))
		r.PerSecond["max_lock_waits"] = max(r.PerSecond["max_lock_waits"], float64(s.LockWaits))
	}
	return r
}

// NATSRates computes message and byte rates between the first and last
// sample.
func NATSRates(samples []NATSSample) Rates {
	if len(samples) < 2 {
		return Rates{NA: "fewer than two NATS samples"}
	}
	first, last := samples[0], samples[len(samples)-1]
	secs := last.At.Sub(first.At).Seconds()
	if secs <= 0 {
		return Rates{NA: "NATS samples share one time"}
	}
	return Rates{PerSecond: map[string]float64{
		"in_msgs_per_s":   float64(last.InMsgs-first.InMsgs) / secs,
		"out_msgs_per_s":  float64(last.OutMsgs-first.OutMsgs) / secs,
		"in_bytes_per_s":  float64(last.InBytes-first.InBytes) / secs,
		"out_bytes_per_s": float64(last.OutBytes-first.OutBytes) / secs,
	}}
}
