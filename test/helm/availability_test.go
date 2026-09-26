package helm

import (
	"fmt"
	"strings"
	"testing"
)

func TestDisruptionBudgetsKeepAllButOne(t *testing.T) {
	objs := render(t)
	for _, c := range []string{"api", "worker", "pdp"} {
		p := find(t, objs, "PodDisruptionBudget", "eacp-"+c)
		if get(p, "spec", "maxUnavailable") != 1 ||
			get(p, "spec", "selector", "matchLabels", "app.kubernetes.io/component") != c {
			t.Errorf("pdb %s = %v", c, p["spec"])
		}
	}
}

func TestRolloutsNeverDropBelowTheReplicas(t *testing.T) {
	objs := render(t)
	for _, c := range []string{"api", "worker", "pdp"} {
		d := find(t, objs, "Deployment", "eacp-"+c)
		if fmt.Sprint(get(d, "spec", "strategy")) != "map[rollingUpdate:map[maxSurge:1 maxUnavailable:0] type:RollingUpdate]" {
			t.Errorf("%s strategy = %v", c, get(d, "spec", "strategy"))
		}
		if get(d, "spec", "replicas") != 2 {
			t.Errorf("%s replicas = %v", c, get(d, "spec", "replicas"))
		}
		if s := fmt.Sprint(get(podSpec(d), "topologySpreadConstraints")); !strings.Contains(s, "kubernetes.io/hostname") {
			t.Errorf("%s is not spread across nodes: %s", c, s)
		}
	}
	for c, grace := range map[string]int{"api": 30, "worker": 330, "pdp": 30} {
		if g := podSpec(find(t, objs, "Deployment", "eacp-"+c))["terminationGracePeriodSeconds"]; g != grace {
			t.Errorf("%s grace = %v, want %d", c, g, grace)
		}
	}
}

func TestGraceMustCoverTheDrain(t *testing.T) {
	for set, msg := range map[string]string{
		"worker.terminationGracePeriodSeconds=324": "worker.terminationGracePeriodSeconds must be at least",
		"api.terminationGracePeriodSeconds=24":     "api.terminationGracePeriodSeconds must be at least",
		"pdp.terminationGracePeriodSeconds=24":     "pdp.terminationGracePeriodSeconds must be at least",
		"worker.maxCallSeconds=400":                "worker.terminationGracePeriodSeconds must be at least",
	} {
		if out, err := renderErr(t, "--set", set); err == nil || !strings.Contains(out, msg) {
			t.Errorf("--set %s: err = %v, output lacks %q", set, err, msg)
		}
	}
	if _, err := renderErr(t, "--set", "worker.terminationGracePeriodSeconds=325"); err != nil {
		t.Errorf("grace exactly delay+timeout+call refused: %v", err)
	}
}

// Every server keeps serving for the shutdown delay after SIGTERM, while
// each node stops routing to it; the PDP included, or the API's call to a
// stopping PDP is refused (ADR-029 §3, §5).
func TestEveryServerDrainsOnShutdown(t *testing.T) {
	for _, set := range [][]string{nil, {"--set", "shutdown.delay=20s", "--set", "shutdown.timeout=5s"}} {
		objs := render(t, set...)
		delay, timeout := "10s", "15s"
		if set != nil {
			delay, timeout = "20s", "5s"
		}
		for c, prefix := range map[string]string{"api": "EACP_", "worker": "EACP_", "pdp": "AGT_PDP_"} {
			d := find(t, objs, "Deployment", "eacp-"+c)
			for name, want := range map[string]string{prefix + "SHUTDOWN_DELAY": delay, prefix + "SHUTDOWN_TIMEOUT": timeout} {
				if got := envDefs(t, d, name); len(got) != 1 || got[0] != want {
					t.Errorf("%v: %s %s = %v, want [%s]", set, c, name, got, want)
				}
			}
		}
	}
}

// Go's resolver waits its resolv.conf timeout (5 s unless set) before it
// resends a lost query, the whole budget of a PDP call (EACP_PDP_TIMEOUT);
// DNS packets get lost while pods churn on a node. The Go services resend
// after 1 s instead (ADR-029 §5).
func TestGoServicesResendALostDNSQueryAfterOneSecond(t *testing.T) {
	objs := render(t)
	for _, c := range []string{"api", "worker"} {
		got := fmt.Sprint(get(podSpec(find(t, objs, "Deployment", "eacp-"+c)), "dnsConfig"))
		if got != "map[options:[map[name:timeout value:1] map[name:attempts value:3]]]" {
			t.Errorf("%s dnsConfig = %s", c, got)
		}
	}
}

func TestAutoscalingIsOffByDefault(t *testing.T) {
	if hpas := all(render(t), "HorizontalPodAutoscaler"); len(hpas) != 0 {
		t.Fatalf("HPAs rendered by default: %d", len(hpas))
	}
	objs := render(t, "--set", "autoscaling.enabled=true")
	for _, c := range []string{"api", "worker"} {
		h := find(t, objs, "HorizontalPodAutoscaler", "eacp-"+c)
		if get(h, "spec", "scaleTargetRef", "name") != "eacp-"+c || get(h, "spec", "minReplicas") != 2 ||
			!strings.Contains(fmt.Sprint(get(h, "spec", "metrics")), "cpu") {
			t.Errorf("hpa %s = %v", c, h["spec"])
		}
		if get(find(t, objs, "Deployment", "eacp-"+c), "spec", "replicas") != nil {
			t.Errorf("%s sets replicas while an HPA owns them", c)
		}
	}
}
