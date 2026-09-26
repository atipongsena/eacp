package demo

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tenantK is the disruption run's tenant; the local connector-secrets
// manifest binds the Fake ERP credential to it.
const tenantK = "00000000-0000-4000-8000-0000000000e2"

// TestKubernetesDisruption shows ADR-029 on a 2-node cluster (Phase 23b):
// purchases keep executing, one PO each, while the API rolls, the workers
// scale out and a node drains, and the API never stops answering. Then it
// checks the chart's isolation and pod security from inside the cluster.
func TestKubernetesDisruption(t *testing.T) {
	if os.Getenv("EACP_DEMO_PLATFORM") != "k8s" {
		t.Skip("set EACP_DEMO_PLATFORM=k8s and run scripts/k8s-e2e.sh (docs/KUBERNETES.md)")
	}
	d := newDemo(t, tenantK)
	d.subject = "carol@initech.test"
	k := d.p.(*k8sPlatform)

	d.step("K0. Tenant Initech with the Slice A cast, an allow-ERP policy and the procurement agent")
	d.tenantWithCast("initech", "Initech", []member{
		{"erin", "registry_editor"}, {"rita", "registry_approver"}, {"ravi", "registry_approver"},
		{"otto", "operator"}, {"carol", "approver"}, {"audra", "auditor"},
	})
	policy := d.must(201, "alice", "POST", "/v1/policies", map[string]any{"content": json.RawMessage(`{
		"format_version": 1, "rules": [
		{"id": "routine", "match": {"target": "erp"}, "verdict": "allow", "reason": "routine purchase"}]}`)})
	d.must(204, "bob", "POST", "/v1/policies/"+policy["id"].(string)+"/activate", map[string]any{"reason": "reviewed"})
	d.register()

	node := strings.TrimSpace(k.must("get", "nodes", "-l", "!node-role.kubernetes.io/control-plane",
		"-o", "jsonpath={.items[0].metadata.name}"))
	if node == "" {
		t.Fatal("the disruption run needs a second (worker) node")
	}
	t.Cleanup(func() {
		_, _ = k.kubectl("uncordon", node)
		_, _ = k.kubectl("-n", "eacp", "scale", "deploy/eacp-worker", "--replicas=2")
	})

	d.step("K1. 30 purchases while the API rolls, the workers scale to 3 and node " + node + " drains")
	stopProbe, probed := probe(d.api + "/healthz")
	const n = 30
	type submitted struct {
		code int
		body map[string]any
	}
	results := make([]submitted, n)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range n {
			payload := map[string]any{"amount": 100}
			if i%2 == 1 { // a call in flight while pods move
				payload["scenario"], payload["delay_ms"] = "slow_response", 1500
			}
			code, body := d.post(fmt.Sprintf("k8s-disruption-%02d", i), "purchase", "erp.create_po", payload)
			results[i] = submitted{code, body}
			time.Sleep(200 * time.Millisecond)
		}
	})
	began := time.Now()
	k.must("-n", "eacp", "rollout", "restart", "deploy/eacp-api")
	k.must("-n", "eacp", "rollout", "status", "deploy/eacp-api", "--timeout=5m")
	d.logf("API rolled (maxUnavailable 0, maxSurge 1) in %v", time.Since(began).Round(time.Second))
	k.must("-n", "eacp", "scale", "deploy/eacp-worker", "--replicas=3")
	k.must("-n", "eacp", "rollout", "status", "deploy/eacp-worker", "--timeout=5m")
	d.logf("workers scaled to 3")
	began = time.Now()
	k.must("drain", node, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=5m")
	d.logf("node %s drained within its PodDisruptionBudgets in %v", node, time.Since(began).Round(time.Second))
	k.must("uncordon", node)
	wg.Wait()

	ids := make([]string, n)
	for i, r := range results {
		id, _ := r.body["id"].(string)
		if (r.code != 200 && r.code != 202) || id == "" {
			t.Fatalf("purchase %d = %d %v", i, r.code, r.body)
		}
		ids[i] = id
	}
	for _, id := range ids {
		d.until(id, "SUCCEEDED")
	}
	pos := d.committedPOs()
	for _, id := range ids {
		if key := d.action(id)["operation_key"].(string); pos[key] != 1 {
			t.Fatalf("ERP holds %d purchase orders for %s, want exactly 1", pos[key], key)
		}
	}
	d.logf("%d purchases SUCCEEDED with exactly one PO each", n)
	ok, failed, failures := stopProbe()
	if failed != 0 {
		t.Fatalf("the API failed %d of %d health probes during the disruption: %v", failed, ok+failed, failures)
	}
	d.logf("%d health probes every 100 ms over %v: none failed", ok, probed().Round(time.Second))

	d.step("K2. Isolation: the agent reaches only the API")
	if out, err := d.p.agent("wget", "-q", "-T", "3", "-O-", "http://controlplane-api:8080/healthz"); err != nil {
		t.Fatalf("agent → controlplane-api:8080: %v\n%s", err, out)
	}
	if out, err := d.p.agent("nc", "-z", "-w", "3", "controlplane-api", "8080"); err != nil {
		t.Fatalf("agent → nc controlplane-api 8080: %v\n%s", err, out)
	}
	worker := strings.Fields(k.must("-n", "eacp", "get", "pods", "-l", "app.kubernetes.io/component=worker",
		"-o", "jsonpath={.items[*].status.podIP}"))
	if len(worker) == 0 {
		t.Fatal("no worker pod IP")
	}
	for _, target := range [][2]string{{"eacp-pdp.eacp.svc", "8443"}, {"postgres.eacp-deps.svc", "5432"},
		{"nats.eacp-deps.svc", "4222"}, {"fakeerp", "8090"}, {worker[0], "8081"}} {
		if out, err := d.p.agent("nc", "-z", "-w", "3", target[0], target[1]); err == nil {
			t.Fatalf("the agent reached %s:%s\n%s", target[0], target[1], out)
		}
		d.logf("agent → %s:%s refused", target[0], target[1])
	}
	d.logf("agent → controlplane-api:8080 works (wget and nc)")

	d.step("K3. Pod security: restricted namespace, non-root, read-only root filesystems")
	if got := strings.TrimSpace(k.must("get", "namespace", "eacp",
		"-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)); got != "restricted" {
		t.Fatalf("namespace eacp enforces %q, want restricted", got)
	}
	var pods struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				SecurityContext struct {
					RunAsNonRoot *bool `json:"runAsNonRoot"`
				} `json:"securityContext"`
				Containers []struct {
					Name            string `json:"name"`
					SecurityContext struct {
						ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem"`
					} `json:"securityContext"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(k.must("-n", "eacp", "get", "pods", "-o", "json")), &pods); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if nr := p.Spec.SecurityContext.RunAsNonRoot; nr == nil || !*nr {
			t.Fatalf("pod %s does not require a non-root user", p.Metadata.Name)
		}
		for _, c := range p.Spec.Containers {
			if ro := c.SecurityContext.ReadOnlyRootFilesystem; ro == nil || !*ro {
				t.Fatalf("container %s/%s has a writable root filesystem", p.Metadata.Name, c.Name)
			}
		}
	}
	d.logf("%d pods: runAsNonRoot, every container's root filesystem read-only", len(pods.Items))

	k.must("-n", "eacp", "scale", "deploy/eacp-worker", "--replicas=2")
	k.must("-n", "eacp", "rollout", "status", "deploy/eacp-worker", "--timeout=5m")
	d.step("Kubernetes disruption run complete")
}

// probe GETs url every 100 ms, each on a new connection with a 2 s
// timeout, until stop is called. stop returns the successes, the failures
// and the first few failure reasons; elapsed how long it probed.
func probe(url string) (stop func() (ok, failed int64, failures []string), elapsed func() time.Duration) {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var okN, failN atomic.Int64
	var mu sync.Mutex
	var reasons []string
	fail := func(r string) {
		failN.Add(1)
		mu.Lock()
		if len(reasons) < 5 {
			reasons = append(reasons, time.Now().Format("15:04:05.000")+" "+r)
		}
		mu.Unlock()
	}
	began := time.Now()
	var took time.Duration
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				took = time.Since(began)
				return
			case <-tick.C:
			}
			resp, err := client.Get(url)
			switch {
			case err != nil:
				fail(err.Error())
			case resp.StatusCode != 200:
				resp.Body.Close()
				fail(resp.Status)
			default:
				resp.Body.Close()
				okN.Add(1)
			}
		}
	}()
	stop = func() (int64, int64, []string) {
		close(done)
		<-finished
		mu.Lock()
		defer mu.Unlock()
		return okN.Load(), failN.Load(), reasons
	}
	return stop, func() time.Duration { return took }
}
