package helm

import (
	"fmt"
	"strings"
	"testing"
)

var gatewayOn = []string{"--set", "llmGateway.enabled=true", "--set", "llmGateway.providerSecret=eacp-provider-keys", "--set-json", `llmGateway.providerPeers=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"providers"}},"podSelector":{"matchLabels":{"app":"provider"}}}]`}

func TestStudioGatewayIsOptionalAndIsolated(t *testing.T) {
	for _, o := range render(t) {
		if strings.Contains(fmt.Sprint(get(o, "metadata", "name")), "llm-gateway") {
			t.Fatal("gateway rendered by default")
		}
	}
	args := append(append([]string{}, studioOn...), gatewayOn...)
	objs := render(t, args...)
	spec := podSpec(find(t, objs, "Deployment", "eacp-llm-gateway"))
	if spec["serviceAccountName"] != "eacp-llm-gateway" || spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false {
		t.Fatal("gateway pod identity")
	}
	if find(t, objs, "ServiceAccount", "eacp-llm-gateway")["automountServiceAccountToken"] != false {
		t.Fatal("gateway token")
	}
	if fmt.Sprint(get(spec, "dnsConfig", "options")) != "[map[name:timeout value:1] map[name:attempts value:3]]" {
		t.Fatal("gateway DNS")
	}
	c := list(spec, "containers")[0]
	if fmt.Sprint(get(c, "command")) != "[/llm-gateway]" || get(c, "securityContext", "readOnlyRootFilesystem") != true || get(c, "securityContext", "allowPrivilegeEscalation") != false {
		t.Fatal("gateway hardening")
	}
	refs := secretRefs(spec)
	if fmt.Sprint(refs) != "map[eacp-db-app:[url] eacp-pdp-tls:[ca.pem client.pem client-key.pem] eacp-provider-keys:[provider-secrets.json]]" {
		t.Fatalf("gateway refs = %v", refs)
	}
	if !mountedReadOnly(spec, "/run/secrets/eacp-llm") {
		t.Fatal("writable provider secrets")
	}
	runtime := podSpec(find(t, objs, "Deployment", "eacp-agent-runtime"))
	found := false
	for _, e := range list(list(runtime, "containers")[0], "env") {
		if get(e, "name") == "EACP_RUNTIME_LLM_URL" {
			found = get(e, "value") == "http://eacp-llm-gateway:8083"
		}
	}
	if !found {
		t.Fatal("runtime has no chart-owned gateway origin")
	}
	p := policy(t, objs, "eacp-agent-runtime")
	eg := list(p, "spec", "egress")
	if len(eg) != 2 || !strings.Contains(fmt.Sprint(eg[1]), "component:llm-gateway") || !strings.Contains(fmt.Sprint(eg[1]), "port:8083") {
		t.Fatalf("runtime egress = %v", eg)
	}
	gp := policy(t, objs, "eacp-llm-gateway")
	if len(list(gp, "spec", "egress")) != 3 || !strings.Contains(fmt.Sprint(gp), "providers") || strings.Contains(fmt.Sprint(gp), "component:worker") || strings.Contains(fmt.Sprint(gp), "app:nats") {
		t.Fatalf("gateway policy = %v", gp)
	}
	if !strings.Contains(fmt.Sprint(policy(t, objs, "eacp-pdp")), "component:llm-gateway") {
		t.Fatal("PDP refuses gateway")
	}
	for _, w := range workloads {
		if _, ok := secretRefs(podSpec(find(t, objs, w.kind, w.name)))["eacp-provider-keys"]; ok {
			t.Fatal("provider keys escaped gateway")
		}
	}
	if get(find(t, objs, "Service", "eacp-llm-gateway"), "spec", "type") != "ClusterIP" {
		t.Fatal("gateway public service")
	}
}

func TestStudioGatewayRefusesUnsafeValues(t *testing.T) {
	base := append(append([]string{}, studioOn...), gatewayOn...)
	for key, value := range map[string]string{"llmGateway.providerSecret": "", "llmGateway.providerPeers": "[]", "llmGateway.providerPort": "0", "llmGateway.replicas": "0", "llmGateway.terminationGracePeriodSeconds": "1", "llmGateway.env.EACP_LLM_SECRETS_FILE": "credential", "llmGateway.env.EACP_CONNECTOR_SECRETS_FILE": "credential", "llmGateway.env.EACP_DATABASE_URL": "credential", "studio.env.EACP_RUNTIME_LLM_URL": "http://other", "api.env.EACP_LLM_SECRETS_FILE": "credential"} {
		args := append(append([]string{}, base...), "--set", key+"="+value)
		if value == "[]" {
			args = append(append([]string{}, base...), "--set-json", key+"="+value)
		}
		out, err := renderErr(t, args...)
		if err == nil {
			t.Errorf("rendered unsafe %s", key)
		} else if !strings.Contains(out, strings.Split(key, ".")[0]) {
			t.Errorf("unrelated refusal for %s: %.200s", key, out)
		}
	}
}
