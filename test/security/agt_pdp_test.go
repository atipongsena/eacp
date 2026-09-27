package security

import (
	"os/exec"
	"strings"
	"testing"

	"eacp/integrations/governance/microsoftagt"
)

// pdpProbe connects to the AGT sidecar from inside the pdp network (a
// one-off agt-pdp container, which mounts the development PKI) and GETs
// /healthz, presenting the client certificate only when withCert is true.
func pdpProbe(withCert bool) (string, error) {
	script := `import http.client, ssl, sys
ctx = ssl.create_default_context(cafile="/pki/ca.pem")
if sys.argv[1] == "cert":
    ctx.load_cert_chain("/pki/client.pem", "/pki/client-key.pem")
c = http.client.HTTPSConnection("agt-pdp", 8443, context=ctx, timeout=5)
c.request("GET", "/healthz")
r = c.getresponse()
print(r.status, r.read().decode())
`
	mode := "none"
	if withCert {
		mode = "cert"
	}
	out, err := exec.Command("docker", "compose", "run", "--rm", "--no-deps", "-T", "--entrypoint", "python",
		"agt-pdp", "-c", script, mode).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Positive control (ADR-002 §8): with the client certificate the sidecar
// answers with the pinned protocol and engine versions.
func TestPDPAnswersAClientCertificate(t *testing.T) {
	requireCompose(t)
	out, err := pdpProbe(true)
	for _, want := range []string{"200 ", microsoftagt.Protocol, `"agt":"` + microsoftagt.Pinned.AGT,
		`"acs":"` + microsoftagt.Pinned.ACS, `"opa":"` + microsoftagt.Pinned.OPA} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("mTLS health (err=%v): %s; want %s", err, out, want)
		}
	}
}

// Without a client certificate the TLS handshake is refused, so being on the
// network is not enough to query the PDP.
func TestPDPRefusesAClientWithoutCertificate(t *testing.T) {
	requireCompose(t)
	if out, err := pdpProbe(false); err == nil || strings.Contains(out, "200 ") {
		t.Fatalf("the PDP answered a client without a certificate: %s", out)
	}
}

// The agent runtime has no network path to the PDP either: only the API
// asks for decisions.
func TestAgentCannotReachThePDP(t *testing.T) {
	requireCompose(t)
	if out, err := fromAgent("nc", "-z", "-w", "3", "agt-pdp", "8443"); err == nil {
		t.Fatalf("agent reached the PDP: %q", out)
	}
}

// Only the API and the sidecar mount the PDP PKI, and the API uses the AGT
// provider with the pinned stack.
func TestOnlyTheAPIAndTheSidecarHoldThePDPPKI(t *testing.T) {
	requireCompose(t)
	for _, service := range []string{"controlplane-api", "agt-pdp"} {
		if _, mounts := inspect(t, service); !strings.Contains(mounts, "/pki") {
			t.Fatalf("%s has no PKI mount (%s); the negative checks would be meaningless", service, mounts)
		}
	}
	for _, service := range []string{"execution-worker", "fakeerp", "fakemcp", "fakea2a", "agent", "postgres"} {
		if _, mounts := inspect(t, service); strings.Contains(mounts, "/pki") {
			t.Errorf("%s mounts the PDP PKI: %s", service, mounts)
		}
	}
	env, _ := inspect(t, "controlplane-api")
	if !strings.Contains(env, "EACP_GOVERNANCE_PROVIDER=microsoft-agt") {
		t.Fatalf("controlplane-api is not using the AGT provider: %s", env)
	}
	logs, err := exec.Command("docker", "compose", "logs", "--no-color", "controlplane-api").CombinedOutput()
	if err != nil || !strings.Contains(string(logs), "AGT sidecar ready") {
		t.Fatalf("controlplane-api did not verify the sidecar's pins at startup (err=%v)", err)
	}
}
