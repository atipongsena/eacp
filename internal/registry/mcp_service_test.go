package registry_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/registry"
)

func TestMCPRegistryService(t *testing.T) {
	e := newEnv(t)
	mcp := e.f.MCPConnector(t, "sap-mcp")

	// Before any scan the server is due and has no tools.
	srv := must[registry.MCPServer](t)(e.svc.MCPServer(e.ctx, e.as("audra"), mcp))
	if srv.LastScan != nil || srv.Leased {
		t.Fatalf("new MCP server = %+v", srv)
	}
	_, err := e.svc.MCPServer(e.ctx, e.as("audra"), e.f.ActiveTool(t, "erp", "read").Connector)
	wantErr(t, err, registry.ErrNotFound)

	e.f.Scan(t, mcp, scanResult(getPO, createPO))
	tools := must[[]registry.Tool](t)(e.svc.ListTools(e.ctx, e.as("carol"), mcp))
	if len(tools) != 2 {
		t.Fatalf("tools = %+v", tools)
	}
	get := tools[1]
	if get.Name != "get_po" || get.RemoteName != "get_po" || get.Origin != "discovered" || get.Definition == nil ||
		get.Definition.Seq != 1 || get.Definition.Risk != "initial" || !get.Definition.ReadOnly || get.Executable {
		t.Fatalf("get_po = %+v (definition %+v)", get, get.Definition)
	}

	// Certify through the service: the contract pins the reviewed definition.
	c := registry.Contract{SideEffects: []string{"READ_ONLY"}, IdempotencyMode: "none", ReconciliationLookup: "none",
		ReconciliationConsistency: "none", ProofStandard: "none", MaxAttempts: 1}
	_, err = e.svc.ProposeContract(e.ctx, e.as("erin"), get.ID, c)
	wantErr(t, err, registry.ErrConflict) // no definition pinned
	c.DefinitionID = &get.Definition.ID
	contract := must[uuid.UUID](t)(e.svc.ProposeContract(e.ctx, e.as("erin"), get.ID, c))
	noErr(t, e.svc.ActivateContract(e.ctx, e.as("rita"), get.ID, contract))
	get = must[registry.Tool](t)(e.svc.GetTool(e.ctx, e.as("carol"), get.ID))
	if !get.Executable || !get.ContractMatches || get.ActiveContractID == nil || *get.ActiveContractID != contract {
		t.Fatalf("certified tool = %+v", get)
	}

	// Drift: history grows, the contract stops matching, the tool is quarantined.
	e.f.Scan(t, mcp, scanResult(mcpTool{"get_po", getPOPoison, `{}`}, createPO))
	get = must[registry.Tool](t)(e.svc.GetTool(e.ctx, e.as("carol"), get.ID))
	if get.Executable || get.ContractMatches || get.QuarantinedAt == nil || get.Definition.Risk != "high" {
		t.Fatalf("drifted tool = %+v", get)
	}
	defs := must[[]registry.Definition](t)(e.svc.ToolDefinitions(e.ctx, e.as("carol"), get.ID))
	if len(defs) != 2 || defs[0].Seq != 2 || defs[0].Changes[0] != "description" || len(defs[0].Fingerprint) != 64 {
		t.Fatalf("definitions = %+v", defs)
	}

	// Quarantine and release through the service.
	_, err = e.svc.ReleaseTool(e.ctx, e.as("otto"), get.ID, "fine")
	wantErr(t, err, registry.ErrForbidden)
	get = must[registry.Tool](t)(e.svc.ReleaseTool(e.ctx, e.as("ravi"), get.ID, "reviewed"))
	if get.QuarantinedAt != nil || get.Executable {
		t.Fatalf("released drifted tool = %+v", get)
	}
	_, err = e.svc.QuarantineTool(e.ctx, e.as("otto"), get.ID, "")
	wantErr(t, err, registry.ErrInvalid)
	get = must[registry.Tool](t)(e.svc.QuarantineTool(e.ctx, e.as("otto"), get.ID, "incident 42"))
	if get.QuarantinedAt == nil || get.QuarantineReason != "incident 42" {
		t.Fatalf("quarantined tool = %+v", get)
	}

	// Rescans and scan history.
	_, err = e.svc.RequestScan(e.ctx, e.as("carol"), mcp, "please")
	wantErr(t, err, registry.ErrForbidden)
	srv = must[registry.MCPServer](t)(e.svc.RequestScan(e.ctx, e.as("otto"), mcp, "vendor release"))
	if srv.RequestedAt == nil || srv.RequestReason != "vendor release" {
		t.Fatalf("rescan request = %+v", srv)
	}
	scans := must[[]registry.Scan](t)(e.svc.MCPScans(e.ctx, e.as("audra"), mcp, 10))
	if len(scans) != 2 || scans[0].Outcome != "ok" || scans[0].DefinitionsChanged != 1 || scans[0].ProtocolVersion != "2026-07-28" ||
		string(scans[0].ServerInfo) != `{"name": "sap-mcp", "version": "3.0.0"}` {
		t.Fatalf("scans = %+v", scans)
	}
}
