package studioruntime_test

import (
	"context"
	"encoding/json"
	"github.com/atipongsena/eacp/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"testing"
)

const branchDefinition = `{"schema_version":2,"kind":"agent","inputs":{"employee_id":{"type":"string","max_length":64}},"limits":{"timeout_seconds":120},"steps":[{"id":"choose","kind":"branch","condition":{"left":"{{inputs.employee_id}}","operator":"eq","right":"E-1"},"then":"lookup","else":"decline"},{"id":"lookup","kind":"tool_call","tool":"hr-mcp.get_leave_balance","tool_schema_version":"1","operation":"lookup","target":"hr","resource":"leave_balance","payload":{"employee_id":"{{inputs.employee_id}}"},"next":"answer"},{"id":"decline","kind":"respond","text":"Not eligible"},{"id":"answer","kind":"respond","text":"You have {{steps.lookup.output.structuredContent.days}} days."}]}`

func (r *rig) useDefinition(def string) {
	r.t.Helper()
	code, v := r.as("stella", "POST", "/v1/studio/agents/"+r.agent+"/versions", map[string]any{"definition": json.RawMessage(def), "expected_version_id": r.version})
	r.want(201, code, v)
	r.version = v["id"].(string)
	code, v = r.as("rita", "POST", "/v1/studio/versions/"+r.version+"/approve", map[string]any{"reason": "reviewed graph"})
	r.want(200, code, v)
	r.approveKeys()
}

func TestRuntimeResumesRecordedGraphActionWithoutResubmission(t *testing.T) {
	r := newRig(t)
	r.useDefinition(branchDefinition)
	id := r.start("E-1")
	code, claimed := r.as("rt", "POST", "/v1/studio/runtime/claims", map[string]any{"runtime_id": "old", "master_version": "v1", "lease_seconds": 5, "limit": 1})
	r.want(200, code, claimed)
	c := claimed["runs"].([]any)[0].(map[string]any)
	base := "/v1/studio/runtime/runs/" + id + "/nodes/"
	lease := map[string]any{"runtime_id": "old", "generation": 1, "index": 0}
	code, body := r.as("rt", "POST", base+"begin", lease)
	r.want(200, code, body)
	lease["result"] = map[string]bool{"choice": true}
	code, body = r.as("rt", "POST", base+"complete", lease)
	r.want(204, code, body)
	delete(lease, "result")
	lease["index"] = 1
	code, body = r.as("rt", "POST", base+"begin", lease)
	r.want(200, code, body)
	key, _ := r.master.Key(r.f.Tenant, uuid.MustParse(c["credential_id"].(string)))
	code, action := r.send(key, "POST", "/v1/actions", map[string]string{"Idempotency-Key": "studio:" + id + ":1"}, map[string]any{"subject": "stella@tenant-a.test", "operation": "lookup", "target": "hr", "tool": "hr-mcp.get_leave_balance", "tool_schema_version": "1", "resource": "leave_balance", "payload": map[string]string{"employee_id": "E-1"}})
	r.want(202, code, action)
	lease["action_id"] = action["id"]
	code, body = r.as("rt", "POST", base+"action", lease)
	r.want(204, code, body)
	err := storage.InTenantTx(context.Background(), r.f.Owner, r.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until=now()-interval '1 second' WHERE id=$1`, uuid.MustParse(id))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	r.runOnce()
	if got := r.run(id); got["state"] != "SUCCEEDED" || got["answer"] != "You have 12 days." {
		t.Fatalf("recovered=%v", got)
	}
	if r.actionPosts.Load() != 1 || len(r.conn.payloads()) != 1 {
		t.Fatal("recorded action was resubmitted")
	}
}

func TestRuntimeV2ChoosesOnlyTheFixedBranch(t *testing.T) {
	r := newRig(t)
	r.useDefinition(branchDefinition)
	decline := r.start("E-2")
	if r.runOnce() != 1 {
		t.Fatal("claim count")
	}
	if got := r.run(decline); got["state"] != "SUCCEEDED" || got["answer"] != "Not eligible" {
		t.Fatalf("declined run=%v", got)
	}
	if len(r.conn.payloads()) != 0 {
		t.Fatal("unchosen tool was executed")
	}
	accept := r.start("E-1")
	if r.runOnce() != 1 {
		t.Fatal("claim count")
	}
	if got := r.run(accept); got["state"] != "SUCCEEDED" || got["answer"] != "You have 12 days." {
		t.Fatalf("accepted run=%v", got)
	}
	if len(r.conn.payloads()) != 1 {
		t.Fatal("chosen tool count")
	}
}

func TestRuntimePreviewUsesSamplesOnlyForTools(t *testing.T) {
	r := newRig(t)
	r.useDefinition(branchDefinition)
	code, run := r.as("stella", "POST", "/v1/studio/versions/"+r.version+"/previews", map[string]any{
		"inputs": map[string]string{"employee_id": "E-1"}, "samples": map[string]any{"lookup": map[string]any{"structuredContent": map[string]int{"days": 9}}}})
	r.want(201, code, run)
	if r.runOnce() != 1 {
		t.Fatal("claim count")
	}
	got := r.run(run["id"].(string))
	if got["state"] != "SUCCEEDED" || got["answer"] != "You have 9 days." {
		t.Fatalf("preview=%v", got)
	}
	if len(r.conn.payloads()) != 0 {
		t.Fatal("preview reached a tool")
	}
}
