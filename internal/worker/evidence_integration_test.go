package worker_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/approval"
	"eacp/internal/registry"
)

const reviewPolicy = `{"format_version":1,"rules":[{"id":"review","verdict":"escalate","reason":"needs review",` +
	`"approval":{"quorum":2,"eligible_roles":["approver"],"ttl_seconds":600}}]}`

// Invariants 10 and 17: from an action_id alone, an auditor reconstructs
// governance, approval, execution, reconciliation and outcome, and the
// journal those references come from verifies as one hash chain.
func TestEvidenceReconstructsTheWholeActionFromItsID(t *testing.T) {
	v := newERPEnvWith(t, reviewPolicy)
	ctx := context.Background()
	a := v.submitTo("PENDING_APPROVAL", "create_po", map[string]any{"scenario": "execute_then_timeout", "delay_ms": 300})
	votes := approval.New(v.f.App)
	for _, who := range []string{"amy", "ben"} {
		if _, err := votes.Vote(ctx, registry.Actor{TenantID: v.f.Tenant, PrincipalID: v.f.P[who]},
			*a.ApprovalRequestID, approval.Approve, "PO within budget"); err != nil {
			t.Fatal(err)
		}
	}
	v.sweep() // release under the current policy (T10)
	v.runWorker(1)
	if got, _ := v.reconcile(v.reconciler(3, 100*time.Millisecond), a.ID); got.State != "SUCCEEDED" {
		t.Fatalf("reconciled = %s", got.State)
	}

	ev, err := v.e.Evidence(ctx, v.f.Tenant, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The action and its outcome.
	if ev.Action.ID != a.ID || ev.Action.State != "SUCCEEDED" || ev.Action.ExternalReference != "PO-"+a.ID.String() ||
		ev.Action.DecisionEvidenceID == nil {
		t.Fatalf("action = %+v", ev.Action)
	}
	// Governance: the escalation, then the revalidation it was released under.
	decisions := map[uuid.UUID]bool{}
	var verdicts []string
	for _, d := range ev.Decisions {
		decisions[d.ID] = true
		verdicts = append(verdicts, d.Verdict)
		if d.PolicyVersion != *ev.Action.PolicyVersion || d.EnforcedDigest != ev.Action.EnforcedDigest ||
			d.InputDigest != ev.Action.InputDigest || len(d.Reasons) == 0 {
			t.Fatalf("decision = %+v, action %+v", d, ev.Action)
		}
	}
	if strings.Join(verdicts, ",") != "escalate,escalate" || !decisions[*ev.Action.DecisionEvidenceID] {
		t.Fatalf("decisions = %+v", ev.Decisions)
	}
	// Approval: two separated votes and a grant consumed by this action only.
	if len(ev.Approvals) != 1 {
		t.Fatalf("approvals = %+v", ev.Approvals)
	}
	req := ev.Approvals[0]
	if req.ID != *a.ApprovalRequestID || req.State != "GRANTED" || !decisions[req.DecisionEvidenceID] ||
		req.EnforcedDigest != ev.Action.EnforcedDigest || req.RequiredQuorum != 2 || len(req.Votes) != 2 {
		t.Fatalf("approval = %+v", req)
	}
	for i, who := range []string{"amy", "ben"} {
		if vote := req.Votes[i]; vote.Approver != v.f.P[who] || vote.Decision != "APPROVE" || vote.Reason == "" {
			t.Fatalf("vote %d = %+v", i, vote)
		}
	}
	if g := req.Grant; g == nil || g.ConsumedByActionID == nil || *g.ConsumedByActionID != a.ID ||
		g.EnforcedDigest != ev.Action.EnforcedDigest {
		t.Fatalf("grant = %+v", req.Grant)
	}
	// Execution and reconciliation.
	if len(ev.Attempts) != 1 || ev.Attempts[0].OperationKey != a.OperationKey || ev.Attempts[0].Outcome != "ambiguous" ||
		len(ev.Checks) != 1 || ev.Checks[0].Result != "found" || ev.Checks[0].ExternalReference != "PO-"+a.ID.String() {
		t.Fatalf("attempts %+v checks %+v", ev.Attempts, ev.Checks)
	}

	// The journal: every move of the action, in order, with its actor and
	// reason, and every governance and approval record it refers to.
	var moves []string
	subjects := map[uuid.UUID]bool{}
	var last int64
	for _, j := range ev.Journal {
		if j.Seq <= last || len(j.Hash) != 64 || len(j.PrevHash) != 64 || j.Reason == "" || j.Actor.Kind == "" {
			t.Fatalf("journal entry = %+v", j)
		}
		last = j.Seq
		subjects[j.Subject.ID] = true
		if j.Kind == "action.received" || j.Kind == "action.transition" {
			var d struct {
				To string `json:"to"`
			}
			if err := json.Unmarshal(j.Data, &d); err != nil {
				t.Fatal(err)
			}
			moves = append(moves, d.To)
		}
	}
	want := []string{"RECEIVED", "PENDING_APPROVAL", "AUTHORIZED", "QUEUED", "LEASED", "EXECUTING",
		"UNKNOWN_OUTCOME", "RECONCILING", "SUCCEEDED"}
	if !slices.Equal(moves, want) {
		t.Fatalf("journaled moves = %v, want %v", moves, want)
	}
	for id := range decisions {
		if !subjects[id] {
			t.Errorf("decision %s is not in the journal", id)
		}
	}
	for _, id := range []uuid.UUID{a.ID, req.ID, req.Votes[0].ID, req.Votes[1].ID, req.Grant.ID} {
		if !subjects[id] {
			t.Errorf("%s is not in the journal", id)
		}
	}
	if !ev.Chain.Verified || ev.Chain.Count < last || len(ev.Chain.Head) != 64 {
		t.Fatalf("chain = %+v (last entry %d)", ev.Chain, last)
	}
	if b, _ := json.Marshal(ev); strings.Contains(string(b), v.secret) {
		t.Fatal("the connector credential is in the evidence")
	}

	// Rewriting history afterwards (a superuser editing a vote's reason)
	// does not hide the evidence; it breaks the chain the evidence reports.
	conn, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica;
		UPDATE eacp.audit_events SET payload = convert_to(replace(convert_from(payload, 'UTF8'),
			'PO within budget', 'rubber stamp'), 'UTF8')
		WHERE convert_from(payload, 'UTF8') LIKE '%PO within budget%'`); err != nil {
		t.Fatal(err)
	}
	ev, err = v.e.Evidence(ctx, v.f.Tenant, a.ID)
	if err != nil || ev.Chain.Verified || ev.Chain.Error == "" || len(ev.Journal) == 0 {
		t.Fatalf("after tampering: chain %+v, err %v", ev.Chain, err)
	}
}
