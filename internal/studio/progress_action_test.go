package studio_test

import (
	"context"
	"testing"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/studio"
)

func TestStudioGraphRecordsPendingActionWithoutAdvancing(t *testing.T) {
	f := newFix(t)
	v, ag := approvedDefinition(t, f, "pending-graph", branchDefinition)
	run := f.ID(t, "stella", startSQL, ag, inputs)
	c := f.mustClaim("r1")
	_, err := f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,0)`, run, "r1", c.Generation)
	ok(t, err)
	ok(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,0,'{"choice":true}'::jsonb)`, run, "r1", c.Generation))
	_, err = f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,1)`, run, "r1", c.Generation)
	ok(t, err)
	act := f.action(v, stepKey(run, 1), stella)
	query := `SELECT eacp.studio_node_action($1,$2,$3,1,$4)`
	wantState(t, f.Exec("rt", query, run, "r1", c.Generation+1, act), sqlForbidden)
	ok(t, f.Exec("rt", query, run, "r1", c.Generation, act))
	ok(t, f.Exec("rt", query, run, "r1", c.Generation, act))
	node, err := f.object("rt", `SELECT eacp.studio_node_begin($1,$2,$3,1)`, run, "r1", c.Generation)
	ok(t, err)
	if node["action_id"] != act.String() || node["state"] != "pending" {
		t.Fatal("pending action was not recorded")
	}
	if got := f.scalar(`SELECT current_index::text FROM eacp.studio_runs WHERE id=$1`, run); got != "1" {
		t.Fatal("pending action advanced cursor")
	}
	wantState(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,1,$4::jsonb)`, run, "r1", c.Generation, `{"action_id":"`+act.String()+`"}`), sqlBadState)
	view, err := studio.New(f.App).Run(context.Background(), registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["stella"]}, run, false)
	ok(t, err)
	if len(view.Nodes) != 2 || view.Nodes[0].Kind != "branch" || view.Nodes[0].State != "completed" || view.Nodes[1].ActionID == nil || *view.Nodes[1].ActionID != act {
		t.Fatal("run view did not expose durable metadata")
	}
}
