package worker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/spiffetest"
)

var spiffeTenant = uuid.MustParse("00000000-0000-4000-8000-0000000000a8")

const (
	workerID       = "spiffe://eacp.test/ns/eacp/sa/eacp-worker"
	spiffeEndpoint = "https://erp.internal:8443/v1"
	spiffeSocket   = "unix:///spiffe-workload-api/spire-agent.sock"
)

// spiffeFile is a secrets file with a spiffe block at endpoint and the given
// bindings (whole entries, without tenant_id).
func spiffeFile(endpoint, id string, entries ...string) string {
	var b []string
	for _, e := range entries {
		b = append(b, fmt.Sprintf(`{"tenant_id":%q,%s}`, spiffeTenant, e))
	}
	return fmt.Sprintf(`{"spiffe":{"endpoint":%q,"spiffe_id":%q},"secrets":[%s]}`, endpoint, id, strings.Join(b, ","))
}

const (
	valueSPIFFE = `"secret_ref":"erp-spiffe","host":"erp.internal:8443","value_spiffe":{"audience":"erp-api"}`
	oauthSPIFFE = `"secret_ref":"erp-spiffe-oauth","host":"erp.internal:8443","oauth2":{"token_url":"https://idp.internal/token","client_id":"eacp-worker","client_assertion_spiffe":{"audience":"api://AzureADTokenExchange"}}`
)

func TestSPIFFEBlockLoads(t *testing.T) {
	a := spiffetest.New(t)
	for _, endpoint := range []string{spiffeSocket, a.Addr()} {
		s, err := LoadSecrets(writeFile(t, "secrets.json", spiffeFile(endpoint, workerID, valueSPIFFE, oauthSPIFFE)),
			AllowPlainTokenURL())
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if b := s.Bindings(); len(b) != 2 || b[0].Ref != "erp-spiffe" || b[1].Ref != "erp-spiffe-oauth" {
			t.Fatalf("bindings = %v", b)
		}
	}
	if n := a.Fetches("erp-api") + a.Fetches("api://AzureADTokenExchange"); n != 0 {
		t.Fatalf("loading contacted the agent %d times", n)
	}
}

func TestSPIFFEBlockFailsClosed(t *testing.T) {
	long := strings.Repeat("x", 257)
	cases := map[string]struct {
		file  string
		plain bool
	}{
		"unix with a host":         {file: spiffeFile("unix://host/x", workerID, valueSPIFFE)},
		"unix relative":            {file: spiffeFile("unix:relative", workerID, valueSPIFFE)},
		"unix with a query":        {file: spiffeFile("unix:///x?q=1", workerID, valueSPIFFE)},
		"unix with a fragment":     {file: spiffeFile("unix:///x#f", workerID, valueSPIFFE)},
		"unix without a path":      {file: spiffeFile("unix://", workerID, valueSPIFFE)},
		"http":                     {file: spiffeFile("http://agent", workerID, valueSPIFFE), plain: true},
		"tcp outside development":  {file: spiffeFile("tcp://127.0.0.1:1", workerID, valueSPIFFE)},
		"tcp not loopback":         {file: spiffeFile("tcp://10.0.0.1:1", workerID, valueSPIFFE), plain: true},
		"tcp host name":            {file: spiffeFile("tcp://localhost:1", workerID, valueSPIFFE), plain: true},
		"tcp without a port":       {file: spiffeFile("tcp://127.0.0.1", workerID, valueSPIFFE), plain: true},
		"tcp with a path":          {file: spiffeFile("tcp://127.0.0.1:1/x", workerID, valueSPIFFE), plain: true},
		"empty spiffe_id":          {file: spiffeFile(spiffeSocket, "", valueSPIFFE)},
		"spiffe_id without a path": {file: spiffeFile(spiffeSocket, "spiffe://", valueSPIFFE)},
		"spiffe_id not spiffe":     {file: spiffeFile(spiffeSocket, "https://eacp.test/w", valueSPIFFE)},
		"spiffe_id too long": {file: spiffeFile(spiffeSocket,
			"spiffe://eacp.test/"+strings.Repeat("a", 2049-len("spiffe://eacp.test/")), valueSPIFFE)},
		"empty audience": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","value_spiffe":{"audience":""}`)},
		"audience with a space": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","value_spiffe":{"audience":"a b"}`)},
		"audience too long": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","value_spiffe":{"audience":"`+long+`"}`)},
		"assertion audience empty": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","oauth2":{"token_url":"https://idp/t","client_id":"c","client_assertion_spiffe":{"audience":""}}`)},
		"value_spiffe without the block": {file: fmt.Sprintf(`{"secrets":[{"tenant_id":%q,%s}]}`, spiffeTenant, valueSPIFFE)},
		"client_assertion_spiffe without the block": {file: fmt.Sprintf(`{"secrets":[{"tenant_id":%q,%s}]}`,
			spiffeTenant, oauthSPIFFE)},
		"value_spiffe and value": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","value":"v","value_spiffe":{"audience":"a"}`)},
		"client_assertion_spiffe and client_secret": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","oauth2":{"token_url":"https://idp/t","client_id":"c","client_secret":"s","client_assertion_spiffe":{"audience":"a"}}`)},
		"unknown member": {file: `{"spiffe":{"endpoint":"` + spiffeSocket + `","spiffe_id":"` + workerID +
			`","socket":"x"},"secrets":[{"tenant_id":"` + spiffeTenant.String() + `",` + valueSPIFFE + `}]}`},
		"unknown audience member": {file: spiffeFile(spiffeSocket, workerID,
			`"secret_ref":"x","host":"h","value_spiffe":{"audience":"a","extra":"b"}`)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var opts []LoadOption
			if c.plain {
				opts = append(opts, AllowPlainTokenURL())
			}
			_, err := LoadSecrets(writeFile(t, "secrets.json", c.file), opts...)
			if err == nil {
				t.Fatal("loaded")
			}
			if !strings.HasPrefix(name, "unknown") && strings.Contains(err.Error(), "not valid JSON") {
				t.Fatalf("refused for its shape, not its value: %v", err)
			}
			for _, v := range []string{"erp-api", "AzureADTokenExchange", "eacp-worker", long} {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("error repeats %q: %v", v, err)
				}
			}
		})
	}
}

func TestTCPEndpointOnlyInDevelopment(t *testing.T) {
	for _, endpoint := range []string{"tcp://127.0.0.1:8081", "tcp://[::1]:8081"} {
		if _, err := LoadSecrets(writeFile(t, "secrets.json", spiffeFile(endpoint, workerID, valueSPIFFE)),
			AllowPlainTokenURL()); err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		if _, err := LoadSecrets(writeFile(t, "secrets.json", spiffeFile(endpoint, workerID, valueSPIFFE))); err == nil {
			t.Fatalf("%s loaded outside development", endpoint)
		}
	}
}
