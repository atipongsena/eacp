package helm

import "testing"

func TestA2AIngressURLIsExplicitAndValidated(t *testing.T) {
	defaultSpec := podSpec(find(t, render(t, "--set", "api.a2aPublicURL="), "Deployment", "eacp-api"))
	for _, e := range list(list(defaultSpec, "containers")[0], "env") {
		if get(e, "name") == "EACP_A2A_PUBLIC_URL" {
			t.Fatal("A2A enabled despite blank opt-in")
		}
	}
	args := []string{"--set", "api.a2aPublicURL=https://eacp.example.test/a2a"}
	objs := render(t, args...)
	spec := podSpec(find(t, objs, "Deployment", "eacp-api"))
	found := false
	for _, e := range list(list(spec, "containers")[0], "env") {
		if get(e, "name") == "EACP_A2A_PUBLIC_URL" {
			found = get(e, "value") == "https://eacp.example.test/a2a"
		}
	}
	if !found {
		t.Fatal("explicit A2A URL absent")
	}
	for _, value := range []string{"http://api.test/a2a", "https://user@api.test/a2a", "https://api.test/other", "https://api.test/a2a?x=1", "https://api.test/a2a#x", "https://api.test:0/a2a", "https://api.test:65536/a2a"} {
		_, err := renderErr(t, "--set", "environment=production", "--set", "api.a2aPublicURL="+value)
		if err == nil {
			t.Fatal("unsafe A2A URL rendered")
		}
	}
	_, err := renderErr(t, "--set", "api.env.EACP_A2A_PUBLIC_URL=https://api.test/a2a")
	if err == nil {
		t.Fatal("extra env bypassed A2A value validation")
	}
}
