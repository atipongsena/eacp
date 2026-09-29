package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/registry/registrytest"
	"github.com/atipongsena/eacp/internal/worker"
)

// TestLoadCarriesTheCertifiedDefinition (ADR-032 S3.7): the job of an MCP
// action carries the server's exact tool name and the certified definition
// of the contract's definition_id, byte for byte; an HTTP tool carries
// neither.
func TestLoadCarriesTheCertifiedDefinition(t *testing.T) {
	e := newScanEnv(t, scanCreate)
	e.f.ActivatePolicy(t, registrytest.AllowPolicy)
	runScan(t, e.scanner(t, "scan-a"), 1)

	var tool, def uuid.UUID
	var name string
	e.row(t, `SELECT id, definition_id, name FROM eacp.tools WHERE connector_id = $1 AND remote_name = 'Create.PO'`,
		[]any{e.connector}, &tool, &def, &name)
	if name == "Create.PO" {
		t.Fatalf("the EACP name %q was not derived from the server name", name)
	}
	contract := e.f.ID(t, "erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, $2, '{IRREVERSIBLE_WRITE,FINANCIAL}', 'none', 'none', 'none', 'none',
		 1, 5000) RETURNING id`, tool, def)
	if err := e.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contract, tool); err != nil {
		t.Fatal(err)
	}
	http := e.f.ActiveTool(t, "erp", "get_po")
	agent := e.f.ActiveAgent(t, "buyer", tool, http.Tool)

	store := worker.NewStore(e.f.App, "w1")
	load := func(ref string) worker.Job {
		t.Helper()
		id := e.f.QueuedAction(t, agent.Version, "carol", ref)
		l, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: e.f.Tenant, ActionID: id}, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim %s: %v, %v", ref, ok, err)
		}
		j, err := store.Load(context.Background(), l)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}

	var certified string
	e.row(t, `SELECT definition FROM eacp.tool_definitions WHERE id = $1`, []any{def}, &certified)
	j := load("sap-mcp." + name)
	if j.Protocol != "mcp" || j.RemoteName != "Create.PO" || certified == "" || j.Definition != certified {
		t.Fatalf("MCP job: protocol %q, remote name %q, definition %q, certified %q",
			j.Protocol, j.RemoteName, j.Definition, certified)
	}

	h := load("erp.get_po")
	if h.Protocol != "http" || h.RemoteName != "" || h.Definition != "" {
		t.Fatalf("HTTP job: protocol %q, remote name %q, definition %q", h.Protocol, h.RemoteName, h.Definition)
	}
}

// TestLoadKeepsTheCertifiedDefinitionAfterARescan (ADR-032 S3.5): a rescan
// that records a newer definition moves the tool's definition_id, but the
// job still carries the definition the contract certified, so the worker
// compares the server with what two people reviewed, never with what the
// server said last.
func TestLoadKeepsTheCertifiedDefinitionAfterARescan(t *testing.T) {
	e := newScanEnv(t, scanCreate)
	e.f.ActivatePolicy(t, registrytest.AllowPolicy)
	sc := e.scanner(t, "scan-a")
	runScan(t, sc, 1)

	var tool, def uuid.UUID
	var name, certified string
	var seq int
	e.row(t, `SELECT t.id, t.definition_id, t.name, d.definition, d.seq FROM eacp.tools t
		JOIN eacp.tool_definitions d ON d.id = t.definition_id
		WHERE t.connector_id = $1 AND t.remote_name = 'Create.PO'`,
		[]any{e.connector}, &tool, &def, &name, &certified, &seq)
	contract := e.f.ID(t, "erin", `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, definition_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, $2, '{IRREVERSIBLE_WRITE,FINANCIAL}', 'none', 'none', 'none', 'none',
		 1, 5000) RETURNING id`, tool, def)
	if err := e.f.Exec("rita", `UPDATE eacp.tools SET active_contract_id = $1 WHERE id = $2`, contract, tool); err != nil {
		t.Fatal(err)
	}
	agent := e.f.ActiveAgent(t, "buyer", tool)
	store := worker.NewStore(e.f.App, "w1")
	id := e.f.QueuedAction(t, agent.Version, "carol", "sap-mcp."+name)
	l, ok, err := store.Claim(context.Background(), worker.Candidate{TenantID: e.f.Tenant, ActionID: id}, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v, %v", ok, err)
	}

	// The server changes the tool's description; an operator asks for a rescan.
	e.server.SetTools(`{"name":"Create.PO","description":"Create and pay a purchase order","inputSchema":{"type":"object"}}`)
	if err := e.f.Exec("otto", `UPDATE eacp.mcp_servers SET requested_at = now(), request_reason = 'vendor release'
		WHERE connector_id = $1`, e.connector); err != nil {
		t.Fatal(err)
	}
	runScan(t, sc, 1)
	var current uuid.UUID
	var currentSeq int
	e.row(t, `SELECT t.definition_id, d.seq FROM eacp.tools t JOIN eacp.tool_definitions d ON d.id = t.definition_id
		WHERE t.id = $1`, []any{tool}, &current, &currentSeq)
	if current == def || currentSeq <= seq {
		t.Fatalf("the rescan did not record a newer definition: %s (seq %d), certified %s (seq %d)", current, currentSeq, def, seq)
	}

	j, err := store.Load(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if certified == "" || j.Definition != certified {
		t.Fatalf("after the rescan the job carries %q, the contract certified %q", j.Definition, certified)
	}
}
