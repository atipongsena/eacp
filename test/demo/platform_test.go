package demo

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// platform is where the demo stack runs: docker compose (scripts/demo.sh)
// or a Kubernetes cluster installed by the Helm chart (scripts/k8s-e2e.sh).
// Compose service names are the vocabulary: controlplane-api,
// execution-worker, agt-pdp, postgres, nats, fakeerp and fakemcp.
type platform interface {
	name() string
	eacpctl(args ...string) (string, error) // eacpctl with the owner DSN
	restart(services ...string)
	kill(service string) // SIGKILL every process of it
	stop(service string)
	start(service string)                    // after kill or stop; waits until ready
	agent(args ...string) (string, error)    // run in the stand-in agent
	postgres(args ...string) (string, error) // run in the PostgreSQL container
	logs() string                            // every EACP and dependency log
	erpAudit(token string) ([]byte, error)   // GET fakeerp /v1/audit with the ERP credential
	copyToFakeMCP(local, remote string) error
}

func newPlatform(t *testing.T, root string) platform {
	if os.Getenv("EACP_DEMO_PLATFORM") == "k8s" {
		return &k8sPlatform{t: t, context: env("EACP_DEMO_KUBE_CONTEXT", "eacp-e2e"), replicas: map[string]string{}}
	}
	return &composePlatform{t: t, root: root, project: env("EACP_DEMO_PROJECT", "eacp-demo")}
}

// composePlatform is the compose demo project (scripts/demo.sh).
type composePlatform struct {
	t       *testing.T
	root    string
	project string
}

func (c *composePlatform) name() string { return "compose" }

func (c *composePlatform) run(args ...string) (string, error) {
	base := []string{"compose", "-p", c.project, "-f", filepath.Join(c.root, "docker-compose.yml"),
		"-f", filepath.Join(c.root, "deployments", "demo", "compose.demo.yml")}
	cmd := exec.Command("docker", append(base, args...)...)
	cmd.Dir = c.root
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *composePlatform) must(args ...string) string {
	c.t.Helper()
	out, err := c.run(args...)
	if err != nil {
		c.t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (c *composePlatform) eacpctl(args ...string) (string, error) {
	return c.run(append([]string{"run", "--rm", "migrate", "/eacpctl"}, args...)...)
}
func (c *composePlatform) restart(services ...string) {
	c.t.Helper()
	c.must(append([]string{"restart"}, services...)...)
}
func (c *composePlatform) kill(s string)  { c.t.Helper(); c.must("kill", s) }
func (c *composePlatform) stop(s string)  { c.t.Helper(); c.must("stop", s) }
func (c *composePlatform) start(s string) { c.t.Helper(); c.must("start", s) }
func (c *composePlatform) agent(args ...string) (string, error) {
	return c.run(append([]string{"exec", "-T", "agent"}, args...)...)
}
func (c *composePlatform) postgres(args ...string) (string, error) {
	return c.run(append([]string{"exec", "-T", "postgres"}, args...)...)
}
func (c *composePlatform) logs() string {
	c.t.Helper()
	return c.must("logs", "--no-color", "controlplane-api", "execution-worker", "fakeerp", "fakemcp", "migrate",
		"postgres", "agt-pdp", "nats")
}
func (c *composePlatform) erpAudit(token string) ([]byte, error) {
	cmd := exec.Command("docker", "run", "--rm", "--network", c.project+"_erp", "busybox:1.37", "wget", "-q", "-O-",
		"--header", "Authorization: Bearer "+token, "http://fakeerp:8090/v1/audit")
	return cmd.Output()
}
func (c *composePlatform) copyToFakeMCP(local, remote string) error {
	if out, err := c.run("cp", local, "fakemcp:"+remote); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// k8sPlatform is the chart's release "eacp" in namespace eacp, with the dev
// dependencies of deployments/k8s/dev in eacp-deps and agents.
type k8sPlatform struct {
	t        *testing.T
	context  string
	replicas map[string]string // replicas of a stopped workload
	killed   map[string]bool   // pods deleted by kill, which start waits past
}

// k8sWorkload maps a compose service to its namespace, workload and pod selector.
type k8sWorkload struct{ ns, ref, selector string }

func workload(service string) k8sWorkload {
	eacp := func(c string) k8sWorkload {
		return k8sWorkload{"eacp", "deploy/eacp-" + c, "app.kubernetes.io/instance=eacp,app.kubernetes.io/component=" + c}
	}
	switch service {
	case "controlplane-api":
		return eacp("api")
	case "execution-worker":
		return eacp("worker")
	case "agt-pdp":
		return eacp("pdp")
	case "postgres":
		return k8sWorkload{"eacp-deps", "statefulset/postgres", "app=postgres"}
	case "nats", "fakeerp", "fakemcp":
		return k8sWorkload{"eacp-deps", "deploy/" + service, "app=" + service}
	}
	panic("unknown demo service " + service)
}

func (k *k8sPlatform) name() string { return "k8s" }

func (k *k8sPlatform) kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", k.context}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (k *k8sPlatform) must(args ...string) string {
	k.t.Helper()
	out, err := k.kubectl(args...)
	if err != nil {
		k.t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// eacpctl runs a one-off pod labelled as the migrate component, so the
// chart's NetworkPolicy lets it reach PostgreSQL, under the restricted Pod
// Security Standard, with the owner DSN from its Secret.
func (k *k8sPlatform) eacpctl(args ...string) (string, error) {
	pod := "eacpctl-" + randomSuffix()
	overrides, err := json.Marshal(map[string]any{"spec": map[string]any{
		"automountServiceAccountToken": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []any{map[string]any{
			"name": pod, "image": "eacp:dev", "imagePullPolicy": "Never", "stdin": true,
			"command": append([]string{"/eacpctl"}, args...),
			"env": []any{map[string]any{"name": "EACP_DATABASE_URL", "valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": "eacp-db-owner", "key": "url"}}}},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}}},
		}},
	}})
	if err != nil {
		return "", err
	}
	return k.kubectl("-n", "eacp", "run", pod, "--rm", "-i", "--restart=Never",
		"--image", "eacp:dev", "--image-pull-policy", "Never", "--pod-running-timeout=3m",
		"--labels", "app.kubernetes.io/name=eacp,app.kubernetes.io/instance=eacp,app.kubernetes.io/component=migrate",
		"--overrides", string(overrides))
}

func (k *k8sPlatform) restart(services ...string) {
	k.t.Helper()
	for _, s := range services {
		w := workload(s)
		k.must("-n", w.ns, "rollout", "restart", w.ref)
	}
	for _, s := range services {
		w := workload(s)
		k.must("-n", w.ns, "rollout", "status", w.ref, "--timeout=5m")
	}
}

// kill force-deletes every pod of the service. The kubelet signals the
// containers and kills them within its minimum grace period (about two
// seconds); the controller starts replacements at once.
func (k *k8sPlatform) kill(s string) {
	k.t.Helper()
	w := workload(s)
	if k.killed == nil {
		k.killed = map[string]bool{}
	}
	for _, p := range strings.Fields(k.must("-n", w.ns, "get", "pods", "-l", w.selector,
		"-o", "jsonpath={.items[*].metadata.name}")) {
		k.killed[p] = true
	}
	k.must("-n", w.ns, "delete", "pod", "-l", w.selector, "--grace-period=0", "--force", "--wait=false")
}

// stop scales the workload to zero and waits until none of its pods remain.
func (k *k8sPlatform) stop(s string) {
	k.t.Helper()
	w := workload(s)
	k.replicas[s] = strings.TrimSpace(k.must("-n", w.ns, "get", w.ref, "-o", "jsonpath={.spec.replicas}"))
	k.must("-n", w.ns, "scale", w.ref, "--replicas=0")
	k.waitPods(w, 0)
}

// start undoes stop (scale back) or waits for kill's replacements.
func (k *k8sPlatform) start(s string) {
	k.t.Helper()
	w := workload(s)
	if n, ok := k.replicas[s]; ok {
		delete(k.replicas, s)
		k.must("-n", w.ns, "scale", w.ref, "--replicas="+n)
	}
	want := 0
	fmt.Sscan(k.must("-n", w.ns, "get", w.ref, "-o", "jsonpath={.spec.replicas}"), &want)
	k.waitPods(w, want)
}

// waitPods waits until exactly want pods of w exist, all Ready, none of
// them terminating or deleted by kill.
func (k *k8sPlatform) waitPods(w k8sWorkload, want int) {
	k.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var list struct {
			Items []struct {
				Metadata struct {
					Name              string  `json:"name"`
					DeletionTimestamp *string `json:"deletionTimestamp"`
				} `json:"metadata"`
				Status struct {
					Conditions []struct{ Type, Status string } `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		out := k.must("-n", w.ns, "get", "pods", "-l", w.selector, "-o", "json")
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			k.t.Fatalf("pods of %s: %v", w.ref, err)
		}
		ready, total := 0, len(list.Items)
		for _, p := range list.Items {
			if p.Metadata.DeletionTimestamp != nil || k.killed[p.Metadata.Name] {
				continue
			}
			for _, c := range p.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" {
					ready++
				}
			}
		}
		if total == want && ready == want {
			return
		}
		if time.Now().After(deadline) {
			k.t.Fatalf("%s: %d pods, %d ready, want %d", w.ref, total, ready, want)
		}
		time.Sleep(time.Second)
	}
}

// nodesOf returns the node of every pod in eacp that matches selector and
// is not terminating.
func (k *k8sPlatform) nodesOf(selector string) []string {
	k.t.Helper()
	var pods struct {
		Items []struct {
			Metadata struct {
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(k.must("-n", "eacp", "get", "pods", "-l", selector, "-o", "json")), &pods); err != nil {
		k.t.Fatalf("pods %s: %v", selector, err)
	}
	var nodes []string
	for _, p := range pods.Items {
		if p.Metadata.DeletionTimestamp == nil {
			nodes = append(nodes, p.Spec.NodeName)
		}
	}
	return nodes
}

func (k *k8sPlatform) agent(args ...string) (string, error) {
	return k.kubectl(append([]string{"-n", "agents", "exec", "deploy/agent", "--"}, args...)...)
}

func (k *k8sPlatform) postgres(args ...string) (string, error) {
	return k.kubectl(append([]string{"-n", "eacp-deps", "exec", "postgres-0", "-c", "postgres", "--"}, args...)...)
}

// logs returns the current pods' logs: unlike compose, Kubernetes drops a
// deleted pod's log with the pod.
func (k *k8sPlatform) logs() string {
	k.t.Helper()
	return k.must("-n", "eacp", "logs", "-l", "app.kubernetes.io/instance=eacp", "--all-containers", "--prefix",
		"--tail=-1", "--max-log-requests=20") +
		k.must("-n", "eacp-deps", "logs", "-l", "app in (postgres,nats,fakeerp,fakemcp)", "--all-containers",
			"--prefix", "--tail=-1", "--max-log-requests=20")
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// erpAudit reads the ERP audit through a port-forward: it enters the Fake
// ERP pod's own network namespace, so the NetworkPolicy that keeps the
// agent out does not apply to it, and it needs the ERP credential anyway.
func (k *k8sPlatform) erpAudit(token string) ([]byte, error) {
	cmd := exec.Command("kubectl", "--context", k.context, "-n", "eacp-deps", "port-forward", "svc/fakeerp",
		"--address", "127.0.0.1", "0:8090")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	port := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			if m := forwarding.FindStringSubmatch(s.Text()); m != nil {
				port <- m[1]
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	var p string
	select {
	case p = <-port:
	case <-time.After(30 * time.Second):
		return nil, errors.New("kubectl port-forward to fakeerp did not start")
	}
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+p+"/v1/audit", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("ERP audit: HTTP %d %s", resp.StatusCode, body)
	}
	return body, nil
}

func (k *k8sPlatform) copyToFakeMCP(string, string) error {
	return errors.New("copying into the distroless Fake MCP pod is not supported on k8s")
}
