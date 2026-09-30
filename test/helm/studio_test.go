package helm

import (
	"fmt"
	"strings"
	"testing"
)

// studioOn enables agent-runtime with its two Secrets (ADR-033 Rev 1.2).
var studioOn = []string{"--set", "studio.enabled=true", "--set", "studio.masterSecret=eacp-studio-master",
	"--set", "studio.runtimeKeySecret=eacp-studio-runtime-keys"}

func TestTheRuntimeIsOffByDefault(t *testing.T) {
	objs := render(t)
	for _, o := range objs {
		if name, _ := get(o, "metadata", "name").(string); strings.Contains(name, "agent-runtime") {
			t.Errorf("%s %s rendered with studio off", o["kind"], name)
		}
		if strings.Contains(fmt.Sprint(o), "studio") {
			t.Errorf("%s %v mentions studio with studio off", o["kind"], get(o, "metadata", "name"))
		}
	}
}

func TestTheRuntimeNeedsItsSecrets(t *testing.T) {
	for want, args := range map[string][]string{
		"studio.masterSecret":     {"--set", "studio.enabled=true", "--set", "studio.runtimeKeySecret=k"},
		"studio.runtimeKeySecret": {"--set", "studio.enabled=true", "--set", "studio.masterSecret=m"},
		"studio.masterVersion":    append(append([]string{}, studioOn...), "--set", "studio.masterVersion=V1"),
		"studio.replicas":         append(append([]string{}, studioOn...), "--set", "studio.replicas=0"),
	} {
		out, err := renderErr(t, args...)
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("%s: rendered (err=%v): %.300s", want, err, out)
		}
	}
}

func TestTheRuntimePodIsHardened(t *testing.T) {
	objs := render(t, studioOn...)
	d := find(t, objs, "Deployment", "eacp-agent-runtime")
	spec := podSpec(d)
	if get(d, "spec", "template", "metadata", "labels", "app.kubernetes.io/component") != "agent-runtime" {
		t.Error("component label missing")
	}
	if spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false ||
		spec["serviceAccountName"] != "eacp-agent-runtime" {
		t.Errorf("pod identity = %v %v %v", spec["automountServiceAccountToken"], spec["enableServiceLinks"], spec["serviceAccountName"])
	}
	if sa := find(t, objs, "ServiceAccount", "eacp-agent-runtime"); sa["automountServiceAccountToken"] != false {
		t.Error("the runtime's service account mounts its token")
	}
	psc := get(spec, "securityContext")
	if get(psc, "runAsNonRoot") != true || get(psc, "runAsUser") != 65532 || get(psc, "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("pod securityContext = %v", psc)
	}
	if fmt.Sprint(get(spec, "dnsConfig", "options")) != "[map[name:timeout value:1] map[name:attempts value:3]]" {
		t.Errorf("dnsConfig = %v", get(spec, "dnsConfig"))
	}
	cs := list(spec, "containers")
	if len(cs) != 1 {
		t.Fatalf("containers = %v", cs)
	}
	c := cs[0]
	sc := get(c, "securityContext")
	if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true ||
		fmt.Sprint(get(sc, "capabilities", "drop")) != "[ALL]" {
		t.Errorf("container securityContext = %v", sc)
	}
	if get(c, "readinessProbe", "httpGet", "path") != "/readyz" || get(c, "livenessProbe") == nil || get(c, "startupProbe") == nil ||
		get(c, "resources", "limits", "memory") == nil || fmt.Sprint(get(c, "command")) != "[/agent-runtime]" {
		t.Errorf("container = %v", c)
	}
	env := map[string]string{}
	for _, e := range list(c, "env") {
		env[fmt.Sprint(get(e, "name"))] = fmt.Sprint(get(e, "value"))
		if get(e, "valueFrom") != nil {
			t.Errorf("env %v comes from a Secret: the runtime reads its secrets as files", get(e, "name"))
		}
	}
	for k, v := range map[string]string{"EACP_API_URL": "http://eacp-api:8080", "EACP_STUDIO_MASTER_FILE": "/run/studio/master",
		"EACP_STUDIO_MASTER_VERSION": "v1", "EACP_RUNTIME_KEY_FILE": "/run/studio/keys", "EACP_ENV": apiEnv(t, objs),
		"EACP_HTTP_ADDR": ":8084"} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	for k := range env {
		if strings.Contains(k, "DATABASE") || strings.Contains(k, "SECRETS_FILE") || strings.Contains(k, "NATS") {
			t.Errorf("the runtime has %s", k)
		}
	}
	refs := secretRefs(spec)
	if fmt.Sprint(refs) != "map[eacp-studio-master:[master] eacp-studio-runtime-keys:[keys]]" {
		t.Errorf("runtime secrets = %v", refs)
	}
	if !mountedReadOnly(spec, "/run/studio") {
		t.Error("/run/studio is not a read-only mount")
	}

	// No other pod gets the runtime's Secrets.
	for _, w := range workloads {
		for n := range secretRefs(podSpec(find(t, objs, w.kind, w.name))) {
			if strings.HasPrefix(n, "eacp-studio") {
				t.Errorf("%s references %s", w.name, n)
			}
		}
	}
}

func TestTheRuntimeReachesOnlyTheAPIInTheCluster(t *testing.T) {
	objs := render(t, studioOn...)
	p := policy(t, objs, "eacp-agent-runtime")
	if component(p) != "agent-runtime" || types(p) != "[Ingress Egress]" || get(p, "spec", "ingress") != nil {
		t.Fatalf("runtime policy = %v", p["spec"])
	}
	eg := list(p, "spec", "egress")
	if len(eg) != 1 || fmt.Sprint(get(eg[0], "ports")) != "[map[port:8080 protocol:TCP]]" ||
		fmt.Sprint(get(eg[0], "to")) != "[map[podSelector:map[matchLabels:map[app.kubernetes.io/component:api app.kubernetes.io/instance:eacp app.kubernetes.io/name:eacp]]]]" {
		t.Fatalf("runtime egress = %v", eg)
	}
	// With the API open to any source, its single rule already admits the runtime.
	if in := list(policy(t, objs, "eacp-api"), "spec", "ingress"); len(in) != 1 || get(in[0], "from") != nil {
		t.Fatalf("api ingress = %v", in)
	}
}

func TestANarrowedAPIIngressStillAdmitsTheRuntime(t *testing.T) {
	args := append(append([]string{}, studioOn...), "--set-json", `api.ingress.from=[{"namespaceSelector":{"matchLabels":{"team":"agents"}}}]`)
	in := fmt.Sprint(list(policy(t, render(t, args...), "eacp-api"), "spec", "ingress"))
	if !strings.Contains(in, "team:agents") || !strings.Contains(in, "app.kubernetes.io/component:agent-runtime") {
		t.Fatalf("narrowed api ingress = %s", in)
	}
	// Without the runtime, the narrowed rule stays alone.
	in = fmt.Sprint(list(policy(t, render(t, "--set-json", `api.ingress.from=[{"namespaceSelector":{"matchLabels":{"team":"agents"}}}]`), "eacp-api"), "spec", "ingress"))
	if strings.Contains(in, "agent-runtime") {
		t.Fatalf("api admits the runtime with studio off: %s", in)
	}
}

func TestRuntimeEnvIsValidated(t *testing.T) {
	for _, k := range []string{"EACP_DATABASE_URL", "EACP_CONNECTOR_SECRETS_FILE", "EACP_LLM_SECRETS_FILE", "EACP_API_URL",
		"EACP_STUDIO_MASTER_FILE", "EACP_RUNTIME_KEY_FILE", "EACP_STUDIO_MASTER_VERSION", "EACP_HTTP_ADDR", "EACP_ENV"} {
		if out, err := renderErr(t, append(append([]string{}, studioOn...), "--set", "studio.env."+k+"=x")...); err == nil {
			t.Errorf("studio.env.%s rendered: %.200s", k, out)
		}
	}
	d := find(t, render(t, append(append([]string{}, studioOn...), "--set", "studio.env.EACP_RUNTIME_CONCURRENCY=8")...),
		"Deployment", "eacp-agent-runtime")
	if !strings.Contains(fmt.Sprint(list(podSpec(d), "containers")), "EACP_RUNTIME_CONCURRENCY value:8") {
		t.Error("studio.env was not applied")
	}
}

// apiEnv is the API's EACP_ENV: the runtime runs in the same environment.
func apiEnv(t *testing.T, objs []object) string {
	t.Helper()
	for _, c := range list(podSpec(find(t, objs, "Deployment", "eacp-api")), "containers") {
		for _, e := range list(c, "env") {
			if get(e, "name") == "EACP_ENV" {
				return fmt.Sprint(get(e, "value"))
			}
		}
	}
	t.Fatal("the API has no EACP_ENV")
	return ""
}
