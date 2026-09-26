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
	for c, grace := range map[string]int{"api": 30, "worker": 330} {
		if g := podSpec(find(t, objs, "Deployment", "eacp-"+c))["terminationGracePeriodSeconds"]; g != grace {
			t.Errorf("%s grace = %v, want %d", c, g, grace)
		}
	}
}

func TestGraceMustCoverTheDrain(t *testing.T) {
	for set, msg := range map[string]string{
		"worker.terminationGracePeriodSeconds=324": "worker.terminationGracePeriodSeconds must be at least",
		"api.terminationGracePeriodSeconds=24":     "api.terminationGracePeriodSeconds must be at least",
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
