package bench

import (
	"testing"
	"time"
)

func TestParseDockerStats(t *testing.T) {
	for _, c := range []struct {
		line string
		want ContainerSample
	}{
		{`{"BlockIO":"0B / 0B","CPUPerc":"12.34%","Container":"aab","ID":"aab","MemPerc":"1.00%",` +
			`"MemUsage":"123.5MiB / 7.7GiB","Name":"eacp-bench-controlplane-api-1","NetIO":"0B / 0B","PIDs":"9"}`,
			ContainerSample{Service: "controlplane-api", CPUPercent: 12.34, MemBytes: int64(123.5 * (1 << 20))}},
		{`{"CPUPerc":"250.00%","MemUsage":"1.5GiB / 7.7GiB","Name":"eacp-bench-postgres-1"}`,
			ContainerSample{Service: "postgres", CPUPercent: 250, MemBytes: int64(1.5 * (1 << 30))}},
		{`{"CPUPerc":"0.10%","MemUsage":"512KiB / 7.7GiB","Name":"eacp-bench-nats-1"}`,
			ContainerSample{Service: "nats", CPUPercent: 0.1, MemBytes: 512 << 10}},
	} {
		got, err := ParseDockerStats([]byte(c.line), "eacp-bench")
		if err != nil || got != c.want {
			t.Errorf("ParseDockerStats(%s) = %+v %v, want %+v", c.line, got, err, c.want)
		}
	}
	if _, err := ParseDockerStats([]byte(`{"CPUPerc":"--","MemUsage":"-- / --","Name":"eacp-bench-nats-1"}`), "eacp-bench"); err == nil {
		t.Error("a stopped container's line must not parse as zero")
	}
	if _, err := ParseDockerStats([]byte(`{"CPUPerc":"1%","MemUsage":"1MiB / 2GiB","Name":"eacp-postgres-1"}`), "eacp-bench"); err == nil {
		t.Error("another project's container must be refused")
	}
}

func TestParseVarz(t *testing.T) {
	at := time.Unix(100, 0)
	got, err := ParseVarz([]byte(`{"server_id":"x","in_msgs":10,"out_msgs":20,"in_bytes":300,"out_bytes":400,"mem":1}`), at)
	want := NATSSample{At: at, InMsgs: 10, OutMsgs: 20, InBytes: 300, OutBytes: 400}
	if err != nil || got != want {
		t.Fatalf("ParseVarz = %+v %v, want %+v", got, err, want)
	}
}

func TestRatesFromDeltas(t *testing.T) {
	a := DBSample{At: time.Unix(0, 0), Commits: 1000, BlksHit: 90, BlksRead: 10, Active: 3, LockWaits: 0}
	mid := DBSample{At: time.Unix(5, 0), Commits: 1200, BlksHit: 100, BlksRead: 10, Active: 9, LockWaits: 4}
	b := DBSample{At: time.Unix(10, 0), Commits: 1500, BlksHit: 490, BlksRead: 110, Active: 2, LockWaits: 1}
	r := DBRates([]DBSample{a, mid, b})
	want := map[string]float64{"commits_per_s": 50, "cache_hit_ratio": 0.8, "max_active": 9, "max_lock_waits": 4}
	if r.NA != "" || len(r.PerSecond) != len(want) {
		t.Fatalf("DBRates = %+v, want %v", r, want)
	}
	for k, v := range want {
		if r.PerSecond[k] != v {
			t.Errorf("%s = %v, want %v", k, r.PerSecond[k], v)
		}
	}
	n := NATSRates([]NATSSample{{At: time.Unix(0, 0), InMsgs: 10}, {At: time.Unix(2, 0), InMsgs: 30, OutMsgs: 8, InBytes: 100, OutBytes: 40}})
	if n.PerSecond["in_msgs_per_s"] != 10 || n.PerSecond["out_msgs_per_s"] != 4 ||
		n.PerSecond["in_bytes_per_s"] != 50 || n.PerSecond["out_bytes_per_s"] != 20 {
		t.Fatalf("NATSRates = %+v", n)
	}
	c := AggregateContainers([]ContainerSample{{Service: "postgres", CPUPercent: 10, MemBytes: 5},
		{Service: "postgres", CPUPercent: 30, MemBytes: 7}}, nil)
	if pg := c["postgres"]; pg.MeanCPU != 20 || pg.MaxCPU != 30 || pg.MaxMemBytes != 7 || pg.NA != "" {
		t.Fatalf("postgres = %+v", pg)
	}
	if c["nats"].NA != "no samples" {
		t.Fatalf("a service without samples = %+v, want n/a", c["nats"])
	}
}

func TestAFailedSourceIsNotZero(t *testing.T) {
	c := AggregateContainers(nil, []string{"docker: not found"})
	for _, s := range KnownServices {
		if c[s].NA != "docker: not found" {
			t.Errorf("%s = %+v, want n/a with the failure", s, c[s])
		}
	}
	if r := DBRates([]DBSample{{}}); r.NA == "" {
		t.Error("one database sample cannot give a rate")
	}
	if r := NATSRates(nil); r.NA == "" {
		t.Error("no NATS samples must be n/a")
	}
}
