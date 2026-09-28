package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/connector/mcp"
	"github.com/atipongsena/eacp/internal/jwttest"
	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/worker"
)

const a2aEndpoint = "http://a2a.test:9000/a2a"

// fakeA2A is a Discoverer standing in for the A2A client: it records what
// the scanner gave it and lists one delegate.
type fakeA2A struct {
	mu        sync.Mutex
	endpoints []string
	secrets   []worker.Secret
}

func (f *fakeA2A) Discover(_ context.Context, endpoint string, secret worker.Secret) (worker.Discovery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpoints = append(f.endpoints, endpoint)
	f.secrets = append(f.secrets, secret)
	return worker.Discovery{
		ProtocolVersion: "1.0",
		ServerInfo:      json.RawMessage(`{"name":"Procurement","version":"1.0.0"}`),
		Tools: []worker.DiscoveredTool{{RemoteName: "delegate",
			Definition: `{"agentCard":{"name":"Procurement"},"inputSchema":{"type":"object"},"name":"delegate"}`,
			Display:    `{}`}},
	}, nil
}

func (f *fakeA2A) calls() ([]string, []worker.Secret) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.endpoints...), append([]worker.Secret(nil), f.secrets...)
}

func a2aConnector(t *testing.T, f *registrytest.Fixture) uuid.UUID {
	t.Helper()
	return f.ID(t, "erin", `INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), 'procurement', 'a2a', $1, 'procurement') RETURNING id`, a2aEndpoint)
}

func lastScan(t *testing.T, e scanEnv, connector uuid.UUID) (outcome, class, version string) {
	t.Helper()
	var c, v *string
	e.row(t, `SELECT s.outcome, s.error_class, s.protocol_version FROM eacp.mcp_servers m
		JOIN eacp.mcp_scans s ON s.id = m.last_scan_id WHERE m.connector_id = $1`, []any{connector}, &outcome, &c, &v)
	if c != nil {
		class = *c
	}
	if v != nil {
		version = *v
	}
	return outcome, class, version
}

// TestTheScannerUsesTheConnectorsDiscoverer: an MCP server and an A2A agent
// are each scanned by the discoverer of their own protocol; a scanner with
// no discoverer for a protocol records the scan failed without contacting
// anything.
func TestTheScannerUsesTheConnectorsDiscoverer(t *testing.T) {
	e := newScanEnv(t, scanGetPO)
	a2a := a2aConnector(t, e.f)
	secrets, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[
		{"tenant_id":%q,"secret_ref":"sap-mcp","host":%q,"value":%q},
		{"tenant_id":%q,"secret_ref":"procurement","host":"a2a.test:9000","value":"a2a-scan-canary"}]}`,
		e.f.Tenant, strings.TrimPrefix(e.server.Server.URL, "http://"), scanToken, e.f.Tenant)))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeA2A{}
	sc, err := worker.NewScanner(e.f.App, worker.ScannerOptions{ID: "scan-a", Interval: 15 * time.Minute,
		Timeout: 5 * time.Second, Secrets: secrets,
		Discoverers: map[string]worker.Discoverer{"mcp": mcp.New(), "a2a": fake}})
	if err != nil {
		t.Fatal(err)
	}
	runScan(t, sc, 2)

	endpoints, sent := fake.calls()
	if len(endpoints) != 1 || endpoints[0] != a2aEndpoint || sent[0].Reveal() != "a2a-scan-canary" {
		t.Fatalf("A2A discoverer called with %v", endpoints)
	}
	if outcome, _, version := lastScan(t, e, a2a); outcome != "ok" || version != "1.0" {
		t.Fatalf("A2A scan %s %s", outcome, version)
	}
	if outcome, _, version := lastScan(t, e, e.connector); outcome != "ok" || version != mcp.Modern {
		t.Fatalf("MCP scan %s %s", outcome, version)
	}
	var delegates int
	e.row(t, `SELECT count(*) FROM eacp.tools WHERE connector_id = $1 AND remote_name = 'delegate'`, []any{a2a}, &delegates)
	if delegates != 1 {
		t.Fatalf("%d delegates", delegates)
	}

	// A scanner that only knows MCP records the A2A scan as failed.
	if err := e.f.Exec("otto", `UPDATE eacp.mcp_servers SET requested_at = now(), request_reason = 'check'
		WHERE connector_id = $1`, a2a); err != nil {
		t.Fatal(err)
	}
	mcpOnly, err := worker.NewScanner(e.f.App, worker.ScannerOptions{ID: "scan-b", Interval: 15 * time.Minute,
		Timeout: 5 * time.Second, Secrets: secrets, Discoverer: mcp.New()})
	if err != nil {
		t.Fatal(err)
	}
	runScan(t, mcpOnly, 1)
	if outcome, class, _ := lastScan(t, e, a2a); outcome != "failed" || class != "no_discoverer" {
		t.Fatalf("A2A scan without a discoverer: %s %s", outcome, class)
	}
	if endpoints, _ := fake.calls(); len(endpoints) != 1 {
		t.Fatal("the A2A agent was contacted by a scanner that cannot discover it")
	}

	if _, err := worker.NewScanner(e.f.App, worker.ScannerOptions{ID: "scan-c", Secrets: secrets}); err == nil {
		t.Fatal("a scanner without a discoverer was accepted")
	}
}

// TestAnA2AScanMayUseAnAWSCredential: only MCP servers refuse a signing
// credential; an A2A agent behind AWS IAM is scanned with the keys.
func TestAnA2AScanMayUseAnAWSCredential(t *testing.T) {
	f := registrytest.New(t)
	a2a := a2aConnector(t, f)
	secrets, err := worker.LoadSecrets(secretsFile(t, fmt.Sprintf(`{"secrets":[{"tenant_id":%q,"secret_ref":"procurement",
		"host":"a2a.test:9000","aws":{"role_arn":"arn:aws:iam::123456789012:role/eacp","region":"us-east-1",
		"service":"execute-api","sts_endpoint":%q,"subject_token":{"file":%q}}}]}`,
		f.Tenant, stsFor(t), subjectFile(t, jwttest.New(t), time.Hour))), worker.AllowPlainTokenURL())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeA2A{}
	sc, err := worker.NewScanner(f.App, worker.ScannerOptions{ID: "scan-a", Interval: 15 * time.Minute,
		Timeout: 5 * time.Second, Secrets: secrets, Discoverers: map[string]worker.Discoverer{"a2a": fake}})
	if err != nil {
		t.Fatal(err)
	}
	runScan(t, sc, 1)
	_, sent := fake.calls()
	if len(sent) != 1 || !sent[0].SignsRequests() {
		t.Fatalf("the A2A discoverer got %d credentials, signing %v", len(sent), len(sent) == 1 && sent[0].SignsRequests())
	}
	if outcome, class, _ := lastScan(t, scanEnv{f: f}, a2a); outcome != "ok" {
		t.Fatalf("A2A scan %s %s", outcome, class)
	}
}
