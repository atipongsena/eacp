package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// SchemaVersion is the results file's format version.
const SchemaVersion = 1

// Results is one eacp-bench run against one PDP: the raw material of every
// published figure (MASTER_PLAN §105). It never holds a key.
type Results struct {
	Schema    int               `json:"schema"`
	GitSHA    string            `json:"git_sha"`
	Dirty     bool              `json:"dirty"`
	PDP       string            `json:"pdp"`
	StartedAt time.Time         `json:"started_at"`
	Machine   Machine           `json:"machine"`
	Overrides []Override        `json:"overrides,omitempty"`
	Flags     map[string]string `json:"flags,omitempty"`
	Setup     []SetupTiming     `json:"setup,omitempty"`
	Levels    []Level           `json:"levels,omitempty"`
	LLM       []Step            `json:"llm,omitempty"`
	// MaxSustainable maps an agent count to the highest step rate that was
	// neither saturated nor incomplete; 0 means none was.
	MaxSustainable map[int]float64 `json:"max_sustainable,omitempty"`
}

// Machine describes where the run happened.
type Machine struct {
	HostCPU        string `json:"host_cpu"`
	HostCores      int    `json:"host_cores"`
	HostMemBytes   int64  `json:"host_mem_bytes"`
	DockerCPUs     int    `json:"docker_cpus"`
	DockerMemBytes int64  `json:"docker_mem_bytes"`
	OS             string `json:"os"`
	Go             string `json:"go"`
	Docker         string `json:"docker"`
	Postgres       string `json:"postgres"`
}

// Override is a bench-only setting next to its default.
type Override struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Default string `json:"default"`
}

// SetupTiming is how long registering agents up to Agents took.
type SetupTiming struct {
	Agents  int     `json:"agents"`
	Seconds float64 `json:"seconds"`
}

// Level is every step measured with Agents registered agents.
type Level struct {
	Agents int    `json:"agents"`
	Steps  []Step `json:"steps"`
}

// Step is one measured rate. Summaries with Count 0, a nil budget mean and
// entries in NA are measures that could not be taken.
type Step struct {
	Offered             float64             `json:"offered_per_s"`
	Requests            int                 `json:"requests"`
	Errors              int                 `json:"errors"`
	Throttled           int                 `json:"throttled"`
	DuplicateKeys       int                 `json:"duplicate_keys"`
	ReplayMismatched    int                 `json:"replay_mismatched"`
	ReplayMissing       int                 `json:"replay_missing"`
	PreexistingOpen     int                 `json:"preexisting_open"`
	AdmissionThroughput float64             `json:"admission_per_s"`
	CompletedThroughput float64             `json:"completed_per_s"`
	Admission           Summary             `json:"admission"`
	Idempotency         Summary             `json:"idempotency"`
	Governance          Summary             `json:"governance"`
	QueueWait           Summary             `json:"queue_wait"`
	Lease               Summary             `json:"lease"`
	External            Summary             `json:"external"`
	Overhead            Summary             `json:"overhead"`
	EndToEnd            Summary             `json:"end_to_end"`
	LLMClient           Summary             `json:"llm_client"`
	LLMLedger           Summary             `json:"llm_ledger"`
	Excluded            map[string]int      `json:"excluded,omitempty"`
	Terminal            map[string]int      `json:"terminal,omitempty"`
	Open                int                 `json:"open"`
	BudgetReserveMeanMs *float64            `json:"budget_reserve_mean_ms,omitempty"`
	Containers          map[string]Resource `json:"containers,omitempty"`
	DB                  Rates               `json:"db"`
	NATS                Rates               `json:"nats"`
	Saturated           []string            `json:"saturated,omitempty"`
	Incomplete          bool                `json:"incomplete"`
	NA                  map[string]string   `json:"na,omitempty"`
}

// WriteResults writes r to path as indented JSON. It refuses, before
// writing anything, when the bytes contain any of secrets.
func WriteResults(path string, r Results, secrets []string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	for _, s := range secrets {
		if s != "" && bytes.Contains(raw, []byte(s)) {
			return errors.New("results contain a secret")
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}
