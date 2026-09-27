package registry_test

// Schema-level tests of the A2A protocol (ADR-030, migration 00023). An A2A
// connector reuses the MCP scan machinery: one discovered tool, delegate,
// whose definition carries the Agent Card.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/registry/registrytest"
)

const delegateSchema = `{"additionalProperties":false,"minProperties":1,"properties":{"data":{"type":"object"},"text":{"maxLength":65536,"minLength":1,"type":"string"}},"type":"object"}`

// delegateDef is the canonical delegate definition for a card whose
// description is desc.
func delegateDef(desc string) string {
	return `{"agentCard":{"capabilities":{},"defaultInputModes":["text/plain"],"defaultOutputModes":["text/plain"],` +
		`"description":` + strconvQuote(desc) + `,"name":"Procurement",` +
		`"skills":[{"description":"Buy goods","id":"buy","name":"Buy","tags":["erp"]}],` +
		`"supportedInterfaces":[{"protocolBinding":"JSONRPC","protocolVersion":"1.0","url":"http://a2a.test:9000/a2a"}],` +
		`"version":"1.0.0"},"inputSchema":` + delegateSchema + `,"name":"delegate"}`
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var delegate = mcpTool{"delegate", delegateDef("Buys goods"), `{}`}

func a2aScanResult(version string, tools ...mcpTool) string {
	var r map[string]any
	if err := json.Unmarshal([]byte(scanResult(tools...)), &r); err != nil {
		panic(err)
	}
	r["protocol_version"] = version
	r["server_info"] = map[string]string{"name": "Procurement", "version": "1.0.0"}
	b, _ := json.Marshal(r)
	return string(b)
}

// a2aWriteContractSQL proposes an irreversible delegation contract for tool
// $1 pinning definition $2 with certified no-effect classes $3.
const a2aWriteContractSQL = `INSERT INTO eacp.tool_contracts
	(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, reconciliation_lookup,
	 reconciliation_consistency, proof_standard, no_effect_errors, max_attempts, timeout_ms)
	VALUES (eacp.current_tenant_id(), $1, $2, '{IRREVERSIBLE_WRITE}', 'none', 'none', 'none', 'none',
	 $3::text[], 1, 60000)
	RETURNING id`

func scannedDelegate(t *testing.T, f *registrytest.Fixture) (connector, tool uuid.UUID) {
	t.Helper()
	connector = f.A2AConnector(t, "procurement")
	f.Scan(t, connector, a2aScanResult("1.0", delegate))
	return connector, toolID(t, f, connector, "delegate")
}

func TestA2AConnectorsGetAScanRow(t *testing.T) {
	f := registrytest.New(t)
	a2a := f.A2AConnector(t, "procurement")

	var due bool
	var gen int64
	ownerRow(t, f, `SELECT next_scan_at <= now(), lease_generation FROM eacp.mcp_servers WHERE connector_id = $1`,
		[]any{a2a}, &due, &gen)
	if !due || gen != 0 {
		t.Fatalf("new A2A connector: due %v, generation %d", due, gen)
	}
	bindings := `[{"tenant_id":"` + f.Tenant.String() + `","secret_ref":"procurement","host":"a2a.test:9000"}]`
	var n int
	ownerRow(t, f, `SELECT count(*) FROM eacp.mcp_scans_due($1::jsonb, 10) WHERE connector_id = $2`,
		[]any{bindings, a2a}, &n)
	if n != 1 {
		t.Fatalf("an A2A connector is not due for a scan (%d)", n)
	}
}

func TestA2ADelegateIsDiscoveredNeverDeclared(t *testing.T) {
	f := registrytest.New(t)
	a2a := f.A2AConnector(t, "procurement")

	_, err := f.TryID("erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name, remote_name)
		VALUES (eacp.current_tenant_id(), $1, 'delegate', 'delegate') RETURNING id`, a2a)
	wantState(t, err, sqlBadState)

	// The scanner holding the lease discovers delegate and nothing else.
	gen := f.ScanGeneration(t, a2a) + 1
	ok(t, f.ExecScanner("scan-1", gen, registrytest.ScanClaimSQL, a2a, "scan-1"))
	wantState(t, f.ExecScanner("scan-1", gen, `INSERT INTO eacp.tools (tenant_id, connector_id, name, remote_name)
		VALUES (eacp.current_tenant_id(), $1, 'x', 'buy')`, a2a), sqlCheck)
	ok(t, f.ExecScanner("scan-1", gen, registrytest.ScanRecordSQL, a2a, a2aScanResult("1.0", delegate)))

	var name, origin string
	ownerRow(t, f, `SELECT name, origin FROM eacp.tools WHERE id = $1`, []any{toolID(t, f, a2a, "delegate")},
		&name, &origin)
	if name != "delegate" || origin != "discovered" {
		t.Fatalf("delegate = %q/%q", name, origin)
	}
}

func TestA2AScanRecordsExactlyOneDelegate(t *testing.T) {
	f := registrytest.New(t)
	a2a := f.A2AConnector(t, "procurement")
	gen := f.ScanGeneration(t, a2a) + 1
	ok(t, f.ExecScanner("scan-1", gen, registrytest.ScanClaimSQL, a2a, "scan-1"))

	noCard := mcpTool{"delegate", `{"inputSchema":` + delegateSchema + `,"name":"delegate"}`, `{}`}
	stringCard := mcpTool{"delegate", `{"agentCard":"x","inputSchema":` + delegateSchema + `,"name":"delegate"}`, `{}`}
	other := mcpTool{"buy", strings.Replace(delegateDef("Buys goods"), `"name":"delegate"`, `"name":"buy"`, 1), `{}`}
	var rejected map[string]any
	ok(t, json.Unmarshal([]byte(a2aScanResult("1.0", delegate)), &rejected))
	rejected["rejected"] = []map[string]string{{"remote_name": "buy", "reason": "not a delegate"}}
	withRejected, _ := json.Marshal(rejected)

	for name, result := range map[string]string{
		"no tool":          a2aScanResult("1.0"),
		"two tools":        a2aScanResult("1.0", delegate, other),
		"not delegate":     a2aScanResult("1.0", other),
		"no card":          a2aScanResult("1.0", noCard),
		"card not object":  a2aScanResult("1.0", stringCard),
		"rejected tools":   string(withRejected),
		"MCP date version": a2aScanResult("2026-07-28", delegate),
		"other version":    a2aScanResult("0.3", delegate),
	} {
		t.Run(name, func(t *testing.T) {
			wantState(t, f.ExecScanner("scan-1", gen, registrytest.ScanRecordSQL, a2a, result), sqlCheck)
		})
	}
	// A failed scan lists nothing.
	ok(t, f.ExecScanner("scan-1", gen, registrytest.ScanRecordSQL, a2a,
		`{"outcome":"failed","error_class":"card_invalid","interval_s":900}`))
	f.Scan(t, a2a, a2aScanResult("1.0", delegate))
	var version string
	ownerRow(t, f, `SELECT protocol_version FROM eacp.mcp_scans WHERE connector_id = $1 AND outcome = 'ok'`,
		[]any{a2a}, &version)
	if version != "1.0" {
		t.Fatalf("protocol version %q", version)
	}

	// An MCP server still reports a dated revision.
	mcp := f.MCPConnector(t, "sap-mcp")
	gen = f.ScanGeneration(t, mcp) + 1
	ok(t, f.ExecScanner("scan-1", gen, registrytest.ScanClaimSQL, mcp, "scan-1"))
	wantState(t, f.ExecScanner("scan-1", gen, registrytest.ScanRecordSQL, mcp, a2aScanResult("1.0", getPO)), sqlCheck)
}

func TestA2ADefinitionChangeQuarantinesACertifiedDelegate(t *testing.T) {
	f := registrytest.New(t)
	a2a, tool := scannedDelegate(t, f)
	c := f.ID(t, "erin", a2aWriteContractSQL, tool, currentDefinition(t, f, tool), []string{"a2a_rejected"})
	ok(t, f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, c, tool))

	// Display metadata only: low risk, still certified.
	f.Scan(t, a2a, a2aScanResult("1.0", mcpTool{"delegate", delegate.def, `{"iconUrl":"https://a2a.test/i.png"}`}))
	var risk string
	var quarantined bool
	ownerRow(t, f, `SELECT d.risk, t.quarantined_at IS NOT NULL FROM eacp.tools t
		JOIN eacp.tool_definitions d ON d.id = t.definition_id WHERE t.id = $1`, []any{tool}, &risk, &quarantined)
	if risk != "low" || quarantined {
		t.Fatalf("display change: risk %s, quarantined %v", risk, quarantined)
	}

	// A changed card is a changed tool.
	f.Scan(t, a2a, a2aScanResult("1.0", mcpTool{"delegate", delegateDef("Buys goods and pays"), `{}`}))
	var changes []string
	ownerRow(t, f, `SELECT d.risk, d.changes, t.quarantined_at IS NOT NULL FROM eacp.tools t
		JOIN eacp.tool_definitions d ON d.id = t.definition_id WHERE t.id = $1`, []any{tool}, &risk, &changes, &quarantined)
	if risk != "high" || !quarantined || len(changes) == 0 || changes[0] != "agentCard" {
		t.Fatalf("card change: risk %s, changes %v, quarantined %v", risk, changes, quarantined)
	}
}

func TestA2AContractRules(t *testing.T) {
	f := registrytest.New(t)
	_, tool := scannedDelegate(t, f)
	def := currentDefinition(t, f, tool)

	// The contract pins the current definition.
	_, err := f.TryID("erin", a2aWriteContractSQL, tool, nil, []string{})
	wantState(t, err, sqlBadState)
	// A delegation is sent at most once: A2A defines no idempotency key.
	_, err = f.TryID("erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, idempotency_key_field,
		 reconciliation_lookup, reconciliation_consistency, proof_standard, max_attempts)
		VALUES (eacp.current_tenant_id(), $1, $2, '{REVERSIBLE_WRITE}', 'native', 'key',
		 'none', 'none', 'none', 3) RETURNING id`, tool, def)
	wantState(t, err, sqlCheck)
	// A2A defines no lookup by operation key.
	_, err = f.TryID("erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts)
		VALUES (eacp.current_tenant_id(), $1, $2, '{IRREVERSIBLE_WRITE}', 'none', 'by_operation_key',
		 'eventual', 'best_effort', 1) RETURNING id`, tool, def)
	wantState(t, err, sqlCheck)
	// A delegation is never read-only.
	_, err = f.TryID("erin", mcpReadContractSQL, tool, def)
	wantState(t, err, sqlCheck)
	// Only the classes the A2A connector can prove are certifiable no-effect.
	for _, class := range []string{"http_500", "timeout", "a2a_failed", "a2a_rpc_", "a2a_rpc_1234567", "a2a_rpc_-32001"} {
		_, err = f.TryID("erin", a2aWriteContractSQL, tool, def, []string{"a2a_rejected", class})
		wantState(t, err, sqlCheck)
	}
	f.ID(t, "erin", a2aWriteContractSQL, tool, def,
		[]string{"a2a_rejected", "connection_refused_before_send", "unauthorized", "invalid_payload", "a2a_rpc_32001"})
}

func TestA2AFingerprintIsV2(t *testing.T) {
	f := registrytest.New(t)
	_, tool := scannedDelegate(t, f)
	defSum := sha256.Sum256([]byte(delegate.def))
	want := sha256.Sum256([]byte(strings.Join([]string{"eacp-tool-v2", "a2a", "http://a2a.test:9000/a2a",
		"procurement", "procurement", "delegate", "delegate", hex.EncodeToString(defSum[:])}, "\n")))
	var got []byte
	ownerRow(t, f, `SELECT eacp.tool_fingerprint(tenant_id, id) FROM eacp.tools WHERE id = $1`, []any{tool}, &got)
	if hex.EncodeToString(got) != hex.EncodeToString(want[:]) {
		t.Fatalf("fingerprint %x, want %x", got, want)
	}
}
