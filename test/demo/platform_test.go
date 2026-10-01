package demo

import (
	"bufio"
	"bytes"
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
	"runtime"
	"strings"
	"testing"
	"time"
)

// vaultCLI runs the vault CLI inside the dev Vault against itself with its
// dev-only root token (docker-compose.yml, deployments/k8s/dev/vault.yaml).
var vaultCLI = []string{"env", "VAULT_ADDR=http://127.0.0.1:8200", "VAULT_TOKEN=dev-only-vault-root-token", "vault"}

// platform is where the demo stack runs: docker compose (scripts/demo.sh)
// or a Kubernetes cluster installed by the Helm chart (scripts/k8s-e2e.sh).
// Compose service names are the vocabulary: controlplane-api,
// execution-worker, agt-pdp, postgres, nats, fakeerp, fakemcp, vault, llm-gateway and fakellm.
type platform interface {
	name() string
	eacpctl(args ...string) (string, error) // eacpctl with the owner DSN
	restart(services ...string)
	// armKill stages a SIGKILL of every process of service; the returned
	// func fires it and returns once it landed. Arm it before the call to
	// interrupt is in flight: firing must beat that call's budget.
	armKill(service string) func()
	stop(service string)
	start(service string)                    // after kill or stop; waits until ready
	agent(args ...string) (string, error)    // run in the stand-in agent
	postgres(args ...string) (string, error) // run in the PostgreSQL container
	vault(args ...string) (string, error)    // the vault CLI in the dev Vault, as its root
	logs() string                            // every EACP and dependency log
	erpAudit(token string) ([]byte, error)   // GET fakeerp /v1/audit with the ERP credential
	copyToFakeMCP(local, remote string) error
	mcpCalls(token string) ([]byte, error) // GET fakemcp /v1/calls with the MCP credential
	hrCalls(token string) ([]byte, error)  // GET fakemcp-hr /v1/calls with its credential
	llmAudit(token string) ([]byte, error) // metadata-only provider audit
	// startRuntime writes files (name to content) into agent-runtime's
	// read-only volume and starts it (the Studio demo, compose only).
	startRuntime(files map[string][]byte) error
	runtimeLogs() string
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
func (c *composePlatform) armKill(s string) func() {
	return func() { c.t.Helper(); c.must("kill", s) }
}
func (c *composePlatform) stop(s string)  { c.t.Helper(); c.must("stop", s) }
func (c *composePlatform) start(s string) { c.t.Helper(); c.must("start", s) }
func (c *composePlatform) agent(args ...string) (string, error) {
	return c.run(append([]string{"exec", "-T", "agent"}, args...)...)
}
func (c *composePlatform) postgres(args ...string) (string, error) {
	return c.run(append([]string{"exec", "-T", "postgres"}, args...)...)
}
func (c *composePlatform) vault(args ...string) (string, error) {
	return c.run(append(append([]string{"exec", "-T", "vault"}, vaultCLI...), args...)...)
}
func (c *composePlatform) logs() string {
	c.t.Helper()
	return c.must("logs", "--no-color", "controlplane-api", "execution-worker", "fakeerp", "fakemcp", "fakea2a", "migrate",
		"postgres", "agt-pdp", "nats", "llm-gateway", "fakellm", "fakemcp-hr")
}
func (c *composePlatform) erpAudit(token string) ([]byte, error) {
	cmd := exec.Command("docker", "run", "--rm", "--network", c.project+"_erp", "busybox:1.37", "wget", "-q", "-O-",
		"--header", "Authorization: Bearer "+token, "http://fakeerp:8090/v1/audit")
	return cmd.Output()
}
func (c *composePlatform) mcpCalls(token string) ([]byte, error) {
	cmd := exec.Command("docker", "run", "--rm", "--network", c.project+"_erp", "busybox:1.37", "wget", "-q", "-O-",
		"--header", "Authorization: Bearer "+token, "http://fakemcp:8091/v1/calls")
	return cmd.Output()
}
func (c *composePlatform) hrCalls(token string) ([]byte, error) {
	cmd := exec.Command("docker", "run", "--rm", "--network", c.project+"_erp", "busybox:1.37", "wget", "-q", "-O-",
		"--header", "Authorization: Bearer "+token, "http://fakemcp-hr:8091/v1/calls")
	return cmd.Output()
}
func (c *composePlatform) llmAudit(token string) ([]byte, error) {
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", c.project+"_llm", "busybox:1.37", "sh", "-c",
		`read -r token; wget -q -O- --header "Authorization: Bearer $token" http://fakellm:8093/v1/audit`)
	cmd.Stdin = strings.NewReader(token + "\n")
	return cmd.Output()
}
func (c *composePlatform) startRuntime(files map[string][]byte) error {
	if out, err := c.run("--profile", "studio", "create", "--build", "agent-runtime"); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	for name, content := range files {
		// Through stdin, so no secret is ever on a command line; readable by
		// the runtime's nonroot user only.
		cmd := exec.Command("docker", "run", "--rm", "-i", "-v", c.project+"_studio_runtime:/v", "busybox:1.37", "sh", "-c",
			"cat > /v/"+name+" && chown 65532:65532 /v/"+name+" && chmod 0400 /v/"+name)
		cmd.Stdin = strings.NewReader(string(content))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("write %s: %w: %s", name, err, out)
		}
	}
	if out, err := c.run("--profile", "studio", "start", "agent-runtime"); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}
func (c *composePlatform) runtimeLogs() string {
	c.t.Helper()
	return c.must("--profile", "studio", "logs", "--no-color", "agent-runtime")
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
	killed   map[string]int    // "pod/container" killed by kill: its restart count then
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
	case "agent-runtime", "llm-gateway":
		return eacp(service)
	case "postgres":
		return k8sWorkload{"eacp-deps", "statefulset/postgres", "app=postgres"}
	case "nats", "fakeerp", "fakemcp", "fakemcp-hr", "fakellm":
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

// armKill stages a SIGKILL of every container of the service's pods through
// its node's container runtime, as docker compose kill does: no process gets
// SIGTERM or any time to record what it was doing. (A forced pod delete still
// lets the kubelet send SIGTERM first, and a draining worker whose in-flight
// call the teardown broke records an ambiguous result before it dies.) The
// kubelet restarts the containers in place, in the same pods; start waits
// for them.
//
// Arming lists the pods and opens one shell per node that resolves the
// containers' pids and then waits: firing only writes a newline, so the kill
// lands in milliseconds, well inside the budget of the call it interrupts
// (listing pods and reaching a node took most of a 3 s budget on a loaded
// cluster). The shells are docker exec into the minikube node containers
// (the Docker driver names them after the nodes; scripts/k8s-e2e.sh).
func (k *k8sPlatform) armKill(s string) func() {
	k.t.Helper()
	w := workload(s)
	if k.killed == nil {
		k.killed = map[string]int{}
	}
	byNode := map[string][]string{}
	for _, p := range k.pods(w) {
		for _, c := range p.Status.ContainerStatuses {
			_, id, ok := strings.Cut(c.ContainerID, "://")
			if !ok || c.State.Running == nil || p.Metadata.DeletionTimestamp != nil {
				continue
			}
			k.killed[p.Metadata.Name+"/"+c.Name] = c.RestartCount
			byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], id)
		}
	}
	if len(byNode) == 0 {
		k.t.Fatalf("%s: no running containers to kill", w.ref)
	}
	script := `pids=; for id in "$@"; do pid=$(crictl inspect -o go-template --template '{{.info.pid}}' "$id") && ` +
		`[ "$pid" -gt 1 ] || exit 1; pids="$pids $pid"; done; echo armed; read _ && kill -KILL $pids`
	type shell struct {
		node   string
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		stdout *bufio.Reader
		stderr *strings.Builder
	}
	var shells []shell
	for node, ids := range byNode {
		sh := shell{node: node, stderr: &strings.Builder{}}
		sh.cmd = exec.Command("docker", append([]string{"exec", "-i", node, "sh", "-c", script, "kill"}, ids...)...)
		sh.cmd.Stderr = sh.stderr
		in, err := sh.cmd.StdinPipe()
		if err != nil {
			k.t.Fatal(err)
		}
		out, err := sh.cmd.StdoutPipe()
		if err != nil {
			k.t.Fatal(err)
		}
		sh.stdin, sh.stdout = in, bufio.NewReader(out)
		if err := sh.cmd.Start(); err != nil {
			k.t.Fatalf("arm the kill on %s: %v", node, err)
		}
		shells = append(shells, sh)
	}
	for _, sh := range shells {
		if line, err := sh.stdout.ReadString('\n'); err != nil || strings.TrimSpace(line) != "armed" {
			k.t.Fatalf("arm the kill on %s: %q %v\n%s", sh.node, line, err, sh.stderr)
		}
	}
	return func() {
		k.t.Helper()
		errs := make(chan error, len(shells))
		for _, sh := range shells { // all nodes at once
			go func() {
				_, err := io.WriteString(sh.stdin, "\n")
				_ = sh.stdin.Close()
				_, _ = io.Copy(io.Discard, sh.stdout) // drain before Wait
				if werr := sh.cmd.Wait(); err == nil {
					err = werr
				}
				if err != nil {
					err = fmt.Errorf("SIGKILL on %s: %w\n%s", sh.node, err, sh.stderr)
				}
				errs <- err
			}()
		}
		for range shells {
			if err := <-errs; err != nil {
				k.t.Fatal(err)
			}
		}
	}
}

// stop scales the workload to zero and waits until none of its pods remain.
func (k *k8sPlatform) stop(s string) {
	k.t.Helper()
	w := workload(s)
	k.replicas[s] = strings.TrimSpace(k.must("-n", w.ns, "get", w.ref, "-o", "jsonpath={.spec.replicas}"))
	k.must("-n", w.ns, "scale", w.ref, "--replicas=0")
	k.waitPods(w, 0)
}

// start undoes stop (scale back) or waits for kill's restarted containers.
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

// k8sPod is the part of a pod waitPods and kill read.
type k8sPod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName string `json:"nodeName"`
	} `json:"spec"`
	Status struct {
		Conditions        []struct{ Type, Status string } `json:"conditions"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			ContainerID  string `json:"containerID"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Running *struct{} `json:"running"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (k *k8sPlatform) pods(w k8sWorkload) []k8sPod {
	k.t.Helper()
	var list struct {
		Items []k8sPod `json:"items"`
	}
	if err := json.Unmarshal([]byte(k.must("-n", w.ns, "get", "pods", "-l", w.selector, "-o", "json")), &list); err != nil {
		k.t.Fatalf("pods of %s: %v", w.ref, err)
	}
	return list.Items
}

// waitPods waits until exactly want pods of w exist, all Ready, none of
// them terminating, and every container kill signalled restarted and Ready.
func (k *k8sPlatform) waitPods(w k8sWorkload, want int) {
	k.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		pods := k.pods(w)
		ready := 0
		for _, p := range pods {
			if p.Metadata.DeletionTimestamp != nil {
				continue
			}
			restarted := true
			for _, c := range p.Status.ContainerStatuses {
				if n, ok := k.killed[p.Metadata.Name+"/"+c.Name]; ok && (c.RestartCount <= n || !c.Ready) {
					restarted = false
				}
			}
			for _, c := range p.Status.Conditions {
				if c.Type == "Ready" && c.Status == "True" && restarted {
					ready++
				}
			}
		}
		if len(pods) == want && ready == want {
			for _, p := range pods {
				for _, c := range p.Status.ContainerStatuses {
					delete(k.killed, p.Metadata.Name+"/"+c.Name)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			k.t.Fatalf("%s: %d pods, %d ready, want %d", w.ref, len(pods), ready, want)
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

// studioNetworkProof runs a tokenless stand-in under the runtime's actual
// selectors, against the cluster's enforced policies and dependency listeners.
func (k *k8sPlatform) studioNetworkProof() {
	k.t.Helper()
	pod := "studio-probe-" + randomSuffix()
	script := `set -eu
nc -z -w 3 eacp-api 8080
nc -z -w 3 eacp-llm-gateway 8083
for target in postgres.eacp-deps.svc.cluster.local:5432 nats.eacp-deps.svc.cluster.local:4222 eacp-pdp:8443 fakemcp-hr.eacp-deps.svc.cluster.local:8091 fakellm.eacp-deps.svc.cluster.local:8093; do
  host=${target%:*}; port=${target##*:}
  nslookup "$host" >/dev/null
  if nc -z -w 3 "$host" "$port"; then echo "unexpected runtime access: $target"; exit 1; fi
done
echo 'runtime network isolation verified'`
	overrides, err := json.Marshal(map[string]any{"spec": map[string]any{
		"automountServiceAccountToken": false,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []any{map[string]any{
			"name": pod, "image": "busybox:1.37", "imagePullPolicy": "Never", "stdin": true,
			"command": []string{"sh", "-c", script},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}}},
		}},
	}})
	if err != nil {
		k.t.Fatal(err)
	}
	k.must("-n", "eacp", "run", pod, "--rm", "-i", "--restart=Never", "--image", "busybox:1.37",
		"--image-pull-policy", "Never", "--pod-running-timeout=3m",
		"--labels", "app.kubernetes.io/name=eacp,app.kubernetes.io/instance=eacp,app.kubernetes.io/component=agent-runtime",
		"--overrides", string(overrides))
}

func (k *k8sPlatform) postgres(args ...string) (string, error) {
	return k.kubectl(append([]string{"-n", "eacp-deps", "exec", "postgres-0", "-c", "postgres", "--"}, args...)...)
}

func (k *k8sPlatform) vault(args ...string) (string, error) {
	return k.kubectl(append(append([]string{"-n", "eacp-deps", "exec", "deploy/vault", "--"}, vaultCLI...), args...)...)
}

// logs returns the current pods' logs: unlike compose, Kubernetes drops a
// deleted pod's log with the pod.
func (k *k8sPlatform) logs() string {
	k.t.Helper()
	return k.must("-n", "eacp", "logs", "-l", "app.kubernetes.io/instance=eacp", "--all-containers", "--prefix",
		"--tail=-1", "--max-log-requests=20") +
		k.must("-n", "eacp-deps", "logs", "-l", "app in (postgres,nats,fakeerp,fakemcp,fakemcp-hr,fakellm)", "--all-containers",
			"--prefix", "--tail=-1", "--max-log-requests=20")
}

var forwarding = regexp.MustCompile(`Forwarding from 127\.0\.0\.1:(\d+)`)

// erpAudit reads the ERP audit through a port-forward: it enters the Fake
// ERP pod's own network namespace, so the NetworkPolicy that keeps the
// agent out does not apply to it, and it needs the ERP credential anyway.
func (k *k8sPlatform) erpAudit(token string) ([]byte, error) {
	return k.forwardedGet("fakeerp", 8090, "/v1/audit", token)
}

func (k *k8sPlatform) forwardedGet(service string, target int, path, token string) ([]byte, error) {
	cmd := exec.Command("kubectl", "--context", k.context, "-n", "eacp-deps", "port-forward", "svc/"+service,
		"--address", "127.0.0.1", fmt.Sprintf("0:%d", target))
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
		return nil, errors.New("kubectl port-forward to dependency did not start")
	}
	req, err := http.NewRequest("GET", "http://127.0.0.1:"+p+path, nil)
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
		return nil, fmt.Errorf("dependency audit: HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func (k *k8sPlatform) mcpCalls(string) ([]byte, error) {
	return nil, errors.New("reading the Fake MCP call log is not supported on k8s")
}

func (k *k8sPlatform) hrCalls(token string) ([]byte, error) {
	return k.forwardedGet("fakemcp-hr", 8091, "/v1/calls", token)
}
func (k *k8sPlatform) llmAudit(token string) ([]byte, error) {
	return k.forwardedGet("fakellm", 8093, "/v1/audit", token)
}

func (k *k8sPlatform) startRuntime(files map[string][]byte) error {
	dir := k.t.TempDir()
	for name, content := range files {
		secret := "eacp-studio-runtime-keys"
		if name == "master" {
			secret = "eacp-studio-master"
		} else if name != "keys" {
			return errors.New("unexpected runtime file")
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		create := exec.Command("kubectl", "--context", k.context, "-n", "eacp", "create", "secret", "generic", secret, "--from-file="+name+"="+path, "--dry-run=client", "-o", "yaml")
		data, err := create.Output()
		if err != nil {
			return errors.New("prepare runtime Secret failed")
		}
		apply := exec.Command("kubectl", "--context", k.context, "apply", "-f", "-")
		apply.Stdin = bytes.NewReader(data)
		if err = apply.Run(); err != nil {
			return errors.New("apply runtime Secret failed")
		}
	}
	helm := env("EACP_DEMO_HELM", "")
	if helm == "" {
		helm = "helm"
		name := "helm"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		p := filepath.Join("..", "..", ".tools", name)
		if _, err := os.Stat(p); err == nil {
			helm = p
		}
	}
	cmd := exec.Command(helm, "upgrade", "eacp", filepath.Join("..", "..", "deployments", "helm", "eacp"), "-n", "eacp", "--kube-context", k.context, "--reuse-values", "--set", "studio.enabled=true", "--set", "studio.masterSecret=eacp-studio-master", "--set", "studio.runtimeKeySecret=eacp-studio-runtime-keys", "--set", "studio.env.EACP_RUNTIME_ROTATE_INTERVAL=5s", "--wait", "--timeout", "5m")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("enable runtime: %w: %s", err, out)
	}
	return nil
}

func (k *k8sPlatform) runtimeLogs() string {
	return k.must("-n", "eacp", "logs", "-l", "app.kubernetes.io/component=agent-runtime", "--all-containers", "--prefix", "--tail=-1")
}

func (k *k8sPlatform) copyToFakeMCP(string, string) error {
	return errors.New("copying into the distroless Fake MCP pod is not supported on k8s")
}
