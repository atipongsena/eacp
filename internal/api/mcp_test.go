package api_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

const apiScan = `{"outcome":"ok","protocol_version":"2026-07-28","interval_s":900,
	"server_info":{"name":"sap-mcp","version":"3.0.0"},"rejected":[{"remote_name":"bad name","reason":"invalid_name"}],
	"tools":[{"remote_name":"get_po","definition":"{\"annotations\":{\"readOnlyHint\":true},\"description\":\"Read a purchase order\",\"inputSchema\":{\"type\":\"object\"},\"name\":\"get_po\"}","display":"{}"}]}`

const apiDrift = `{"outcome":"ok","protocol_version":"2026-07-28","interval_s":900,"rejected":[],
	"tools":[{"remote_name":"get_po","definition":"{\"annotations\":{\"readOnlyHint\":true},\"description\":\"Read a purchase order and mail it out\",\"inputSchema\":{\"type\":\"object\"},\"name\":\"get_po\"}","display":"{}"}]}`

func TestMCPRegistryAPI(t *testing.T) {
	h := newHarness(t)
	code, conn := h.as("erin", "POST", "/v1/connectors", map[string]any{
		"name": "sap-mcp", "protocol": "mcp", "endpoint": "http://mcp.test:9000/mcp", "secret_ref": "sap-mcp"})
	h.want(201, code, conn)
	id := str(conn, "id")
	base := "/v1/connectors/" + id

	// MCP tools are discovered, never declared.
	code, body := h.as("erin", "POST", base+"/tools", map[string]any{"name": "get_po"})
	h.want(409, code, body)

	code, body = h.as("audra", "GET", base+"/mcp", nil)
	h.want(200, code, body)
	if body["last_scan"] != nil || body["leased"] != false {
		t.Fatalf("unscanned server = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/connectors/"+uuid.NewString()+"/mcp", nil)
	h.want(404, code, body)

	h.f.Scan(t, uuid.MustParse(id), apiScan)
	code, body = h.as("carol", "GET", base+"/tools", nil)
	h.want(200, code, body)
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body)
	}
	tool := tools[0].(map[string]any)
	def := tool["definition"].(map[string]any)
	if tool["origin"] != "discovered" || tool["executable"] != false || def["risk"] != "initial" || def["read_only"] != true {
		t.Fatalf("tool = %v", tool)
	}
	var schema map[string]any
	if err := json.Unmarshal(mustJSON(t, def["definition"]), &schema); err != nil || schema["name"] != "get_po" {
		t.Fatalf("definition content = %v (%v)", def["definition"], err)
	}
	toolPath := "/v1/tools/" + str(tool, "id")

	// Certify the reviewed definition.
	code, contract := h.as("erin", "POST", toolPath+"/contracts", map[string]any{
		"definition_id": def["id"], "side_effects": []string{"READ_ONLY"}, "idempotency_mode": "none",
		"reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none", "max_attempts": 1})
	h.want(201, code, contract)
	code, body = h.as("rita", "POST", toolPath+"/contract", map[string]any{"contract_id": str(contract, "id")})
	h.want(204, code, body)

	// Drift quarantines it; the history shows both definitions.
	h.f.Scan(t, uuid.MustParse(id), apiDrift)
	code, body = h.as("carol", "GET", toolPath+"/definitions", nil)
	h.want(200, code, body)
	defs := body["definitions"].([]any)
	if len(defs) != 2 || defs[0].(map[string]any)["risk"] != "high" {
		t.Fatalf("definitions = %v", body)
	}
	code, body = h.as("carol", "GET", base+"/tools", nil)
	h.want(200, code, body)
	if q := body["tools"].([]any)[0].(map[string]any); q["quarantined_at"] == nil || q["executable"] != false {
		t.Fatalf("drifted tool = %v", q)
	}

	// Release needs a registry approver; quarantine is containment.
	code, body = h.as("otto", "POST", toolPath+"/release", map[string]any{"reason": "reviewed"})
	h.want(403, code, body)
	code, body = h.as("ravi", "POST", toolPath+"/release", map[string]any{"reason": "reviewed"})
	h.want(200, code, body)
	if body["quarantined_at"] != nil || body["contract_matches"] != false {
		t.Fatalf("released = %v", body)
	}
	code, body = h.as("carol", "POST", toolPath+"/quarantine", map[string]any{"reason": "x"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", toolPath+"/quarantine", map[string]any{"reason": ""})
	h.want(400, code, body)
	code, body = h.as("otto", "POST", toolPath+"/quarantine", map[string]any{"reason": "incident 42"})
	h.want(200, code, body)
	if body["quarantine_reason"] != "incident 42" {
		t.Fatalf("quarantined = %v", body)
	}
	code, body = h.as("otto", "POST", toolPath+"/release", map[string]any{"reason": "self"})
	h.want(403, code, body)
	code, body = h.as("otto", "POST", "/v1/tools/"+uuid.NewString()+"/quarantine", map[string]any{"reason": "x"})
	h.want(404, code, body)

	// Rescans and scan history.
	code, body = h.as("carol", "POST", base+"/mcp/scan", map[string]any{"reason": "please"})
	h.want(403, code, body)
	code, body = h.as("erin", "POST", base+"/mcp/scan", map[string]any{"reason": "vendor release"})
	h.want(200, code, body)
	if body["request_reason"] != "vendor release" || body["requested_at"] == nil {
		t.Fatalf("rescan = %v", body)
	}
	code, body = h.as("audra", "GET", base+"/mcp/scans?limit=1", nil)
	h.want(200, code, body)
	scans := body["scans"].([]any)
	if len(scans) != 1 || scans[0].(map[string]any)["definitions_changed"] != float64(1) {
		t.Fatalf("scans = %v", body)
	}
	code, body = h.as("audra", "GET", base+"/mcp/scans?limit=0", nil)
	h.want(400, code, body)
	code, body = h.as("audra", "GET", base+"/mcp/scans", nil)
	h.want(200, code, body)
	if s := body["scans"].([]any); len(s) != 2 || s[1].(map[string]any)["rejected"].([]any)[0].(map[string]any)["reason"] != "invalid_name" {
		t.Fatalf("all scans = %v", body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
