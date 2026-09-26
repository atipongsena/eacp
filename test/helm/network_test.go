package helm

import (
	"fmt"
	"strings"
	"testing"
)

func policy(t *testing.T, objs []object, name string) object {
	t.Helper()
	return find(t, objs, "NetworkPolicy", name)
}

func types(p object) string { return fmt.Sprint(list(p, "spec", "policyTypes")) }

func component(p object) any {
	return get(p, "spec", "podSelector", "matchLabels", "app.kubernetes.io/component")
}

func TestTheNamespaceDeniesEverythingByDefault(t *testing.T) {
	p := policy(t, render(t), "eacp-default-deny")
	if len(get(p, "spec", "podSelector").(map[string]any)) != 0 || types(p) != "[Ingress Egress]" ||
		get(p, "spec", "ingress") != nil || get(p, "spec", "egress") != nil {
		t.Fatalf("default deny = %v", p["spec"])
	}
}

func TestChartPodsResolveNamesOnly(t *testing.T) {
	p := policy(t, render(t), "eacp-dns")
	s := fmt.Sprint(p["spec"])
	if !strings.Contains(s, "k8s-app:kube-dns") || !strings.Contains(s, "port:53") || types(p) != "[Egress]" {
		t.Fatalf("dns policy = %v", s)
	}
}

func TestTheAPIIsTheFrontDoorAndReachesOnlyItsDependencies(t *testing.T) {
	p := policy(t, render(t), "eacp-api")
	if component(p) != "api" || types(p) != "[Ingress Egress]" {
		t.Fatalf("api policy = %v", p["spec"])
	}
	in := list(p, "spec", "ingress")
	if len(in) != 1 || fmt.Sprint(get(in[0], "ports")) != "[map[port:8080 protocol:TCP]]" || get(in[0], "from") != nil {
		t.Fatalf("api ingress = %v", in)
	}
	eg := fmt.Sprint(list(p, "spec", "egress"))
	for _, want := range []string{"app:postgres", "port:5432", "app:nats", "port:4222",
		"app.kubernetes.io/component:pdp", "port:8443"} {
		if !strings.Contains(eg, want) {
			t.Errorf("api egress lacks %s: %s", want, eg)
		}
	}
	if strings.Contains(eg, "fakeerp") || strings.Contains(eg, "8090") {
		t.Errorf("the API may reach the ERP: %s", eg)
	}
}

func TestOnlyTheWorkerReachesEnterpriseSystems(t *testing.T) {
	objs := render(t)
	w := policy(t, objs, "eacp-worker")
	if component(w) != "worker" || get(w, "spec", "ingress") != nil || types(w) != "[Ingress Egress]" {
		t.Fatalf("worker policy = %v", w["spec"])
	}
	eg := fmt.Sprint(list(w, "spec", "egress"))
	for _, want := range []string{"app:fakeerp", "port:8090", "app:fakemcp", "port:8091", "app:postgres", "app:nats"} {
		if !strings.Contains(eg, want) {
			t.Errorf("worker egress lacks %s: %s", want, eg)
		}
	}
	for _, name := range []string{"eacp-api", "eacp-pdp", "eacp-migrate", "eacp-dns"} {
		if s := fmt.Sprint(policy(t, objs, name)["spec"]); strings.Contains(s, "fakeerp") || strings.Contains(s, "8090") {
			t.Errorf("%s reaches the ERP: %s", name, s)
		}
	}
}

func TestOnlyTheAPIReachesThePDP(t *testing.T) {
	p := policy(t, render(t), "eacp-pdp")
	in := list(p, "spec", "ingress")
	if component(p) != "pdp" || types(p) != "[Ingress Egress]" || get(p, "spec", "egress") != nil || len(in) != 1 ||
		fmt.Sprint(get(in[0], "from")) != "[map[podSelector:map[matchLabels:map[app.kubernetes.io/component:api app.kubernetes.io/instance:eacp app.kubernetes.io/name:eacp]]]]" ||
		fmt.Sprint(get(in[0], "ports")) != "[map[port:8443 protocol:TCP]]" {
		t.Fatalf("pdp policy = %v", p["spec"])
	}
}

func TestMigrationsReachOnlyPostgres(t *testing.T) {
	p := policy(t, render(t), "eacp-migrate")
	eg := list(p, "spec", "egress")
	if component(p) != "migrate" || len(eg) != 1 || !strings.Contains(fmt.Sprint(eg), "app:postgres") {
		t.Fatalf("migrate policy = %v", p["spec"])
	}
}

func TestAPIIngressCanBeNarrowed(t *testing.T) {
	p := policy(t, render(t, "--set-json", `api.ingress.from=[{"namespaceSelector":{"matchLabels":{"team":"agents"}}}]`), "eacp-api")
	if s := fmt.Sprint(list(p, "spec", "ingress")); !strings.Contains(s, "team:agents") {
		t.Fatalf("narrowed ingress = %s", s)
	}
}
