package studio_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/atipongsena/eacp/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (f *fix) admit(version uuid.UUID, body map[string]any) (map[string]any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = storage.InTenantTx(context.Background(), f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(context.Background(), tx, version); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), `SELECT eacp.llm_admit($1::jsonb)`, string(data)).Scan(&raw)
	})
	var out map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func TestStudioKillAndSettlementRaceKeepsContainment(t *testing.T) {
	f, v, run, gen, in := studioLLMSetup(t, true)
	a, err := f.admit(v, in)
	ok(t, err)
	call := uuid.MustParse(a["call_id"].(string))
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errs[0] = f.Exec("otto", killSQL, "run", run, true) }()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = f.ExecSystem("llm_gateway", studioSettleSQL, call, `{"eligible":true}`)
	}()
	close(start)
	wg.Wait()
	for _, err := range errs {
		ok(t, err)
	}
	_, err = f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", gen)
	wantState(t, err, sqlForbidden, sqlBadState)
	state := f.scalar(`SELECT state FROM eacp.studio_runs WHERE id=$1`, run)
	if state == "RUNNING" {
		status, err := f.beat(run, gen)
		ok(t, err)
		if status != "killed" {
			t.Fatal(status)
		}
	}
	ok(t, f.Exec("opal", killSQL, "run", run, false))
	if got := f.scalar(`SELECT eacp.llm_call_killed($1)::text`, call); got != "true" {
		t.Fatal("containment lost after resume")
	}
	if got := f.scalar(`SELECT count(*)::text FROM eacp.studio_node_results WHERE output IS NOT NULL`); got != "0" {
		t.Fatal("contained content retained")
	}
}

func studioLLMSetup(t *testing.T, funded bool) (*fix, uuid.UUID, uuid.UUID, int64, map[string]any) {
	f, _ := builderFix(t)
	v, ag := approvedDefinition(t, f, "llm-run", llmDefinition)
	f.ID(t, "alice", `INSERT INTO eacp.model_prices(tenant_id,provider,model,unit,input_per_mtok,output_per_mtok,reason)
        VALUES(eacp.current_tenant_id(),'openai','triage-upstream','USD',2,8,'test price') RETURNING id`)
	if funded {
		f.FundAgent(t, ag, "USD", "1")
	}
	run := f.ID(t, "stella", startSQL, ag, `{}`)
	c := f.mustClaim("r1")
	node, err := f.object("rt", `SELECT eacp.studio_llm_begin($1,$2,$3,0)`, run, "r1", c.Generation)
	ok(t, err)
	return f, v, run, c.Generation, map[string]any{
		"model_name": "triage", "provider": "openai", "subject": stella, "stream": false, "request_bytes": 100,
		"max_output_tokens": 100, "gateway_id": "g1", "studio_intent_id": node["intent_id"], "studio_runtime_id": "r1", "studio_generation": c.Generation,
		"decision": map[string]any{"id": uuid.NewString(), "bundle_id": uuid.NewString(), "version": 1, "verdict": "allow", "input_digest": "00", "denial": ""},
	}
}

func TestStudioAdmissionRequiresItsCurrentIntent(t *testing.T) {
	f, v, _, _, in := studioLLMSetup(t, true)
	for _, field := range []string{"studio_intent_id", "subject", "studio_runtime_id", "studio_generation", "max_output_tokens", "stream", "model_name", "provider"} {
		bad := make(map[string]any)
		for k, val := range in {
			bad[k] = val
		}
		switch field {
		case "studio_intent_id":
			delete(bad, field)
		case "max_output_tokens":
			bad[field] = 99
		case "studio_generation":
			bad[field] = 0
		case "stream":
			bad[field] = true
		default:
			bad[field] = "wrong"
		}
		_, err := f.admit(v, bad)
		wantState(t, err, sqlForbidden)
	}
	admitted, err := f.admit(v, in)
	ok(t, err)
	if admitted["denial"] != "" || admitted["studio"] == nil {
		t.Fatalf("admission missing Studio binding: %v", admitted)
	}
	_, err = f.admit(v, in)
	wantState(t, err, sqlBadState)
	if got := f.scalar(`SELECT count(*)::text FROM eacp.llm_calls`); got != "1" {
		t.Fatalf("calls=%s", got)
	}
}

func TestStudioLLMRequiresALeafBudget(t *testing.T) {
	f, v, _, _, in := studioLLMSetup(t, false)
	a, err := f.admit(v, in)
	ok(t, err)
	if a["denial"] != "budget_pending" {
		t.Fatalf("denial=%v", a["denial"])
	}
	_, err = f.admit(v, in)
	wantState(t, err, sqlBadState)
}

func TestStudioTakeoverCannotResendAConsumedIntent(t *testing.T) {
	f, v, run, gen, in := studioLLMSetup(t, true)
	f.owner(func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE eacp.studio_runs SET leased_until=now()-interval '1 second' WHERE id=$1`, run)
		return err
	})
	c := f.mustClaim("r2")
	node, err := f.object("rt", `SELECT eacp.studio_llm_begin($1,$2,$3,0)`, run, "r2", c.Generation)
	ok(t, err)
	if node["intent_id"] != in["studio_intent_id"] {
		t.Fatal("intent changed")
	}
	_, err = f.admit(v, in)
	wantState(t, err, sqlForbidden)
	in["studio_runtime_id"], in["studio_generation"] = "r2", c.Generation
	a, err := f.admit(v, in)
	ok(t, err)
	node, err = f.object("rt", `SELECT eacp.studio_llm_begin($1,$2,$3,0)`, run, "r2", c.Generation)
	ok(t, err)
	if node["call_id"] != a["call_id"] {
		t.Fatal("consumed intent lost its call")
	}
	_, err = f.object("rt", `SELECT eacp.studio_llm_begin($1,$2,$3,0)`, run, "r1", gen)
	wantState(t, err, sqlForbidden)
	_, err = f.admit(v, in)
	wantState(t, err, sqlBadState)
}

const studioSettleSQL = `SELECT eacp.studio_llm_settle($1,'succeeded',200,10,0,0,5,true,$2::jsonb,NULL)`

func TestStudioOutputIsAtomicWithSettlementAndPrivate(t *testing.T) {
	f, v, run, gen, in := studioLLMSetup(t, true)
	a, err := f.admit(v, in)
	ok(t, err)
	call := uuid.MustParse(a["call_id"].(string))
	// Invalid output cannot settle spend or create a partial result.
	wantState(t, f.ExecSystem("llm_gateway", studioSettleSQL, call, `{"eligible":"yes"}`), sqlCheck)
	if got := f.scalar(`SELECT state FROM eacp.llm_calls WHERE id=$1`, call); got != "ADMITTED" {
		t.Fatalf("partial settlement=%s", got)
	}
	ok(t, f.ExecSystem("llm_gateway", studioSettleSQL, call, `{"eligible":true}`))
	out, err := f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", gen)
	ok(t, err)
	if out["state"] != "succeeded" || out["output"].(map[string]any)["eligible"] != true {
		t.Fatal("typed output missing")
	}
	wantState(t, f.Exec("rt", `SELECT output FROM eacp.studio_node_results`), sqlForbidden)
	_, err = f.object("rt", `SELECT eacp.studio_node_output($1,$2,$3,0)`, run, "r1", gen+1)
	wantState(t, err, sqlForbidden)
	ok(t, f.Exec("rt", `SELECT eacp.studio_node_complete($1,$2,$3,0,'{}'::jsonb)`, run, "r1", gen))
	ok(t, f.Exec("rt", finishSQL, run, "r1", gen, "SUCCEEDED", "true", nil))
	if got := f.scalar(`SELECT count(*)::text FROM eacp.studio_node_results WHERE output IS NOT NULL`); got != "0" {
		t.Fatal("terminal output not cleared")
	}
}

func TestKilledStudioCallCannotPublishOutput(t *testing.T) {
	f, v, run, gen, in := studioLLMSetup(t, true)
	a, err := f.admit(v, in)
	ok(t, err)
	call := uuid.MustParse(a["call_id"].(string))
	ok(t, f.Exec("otto", killSQL, "run", run, true))
	if got := f.scalar(`SELECT eacp.llm_call_killed($1)::text`, call); got != "true" {
		t.Fatal("run kill did not reach LLM call")
	}
	ok(t, f.ExecSystem("llm_gateway", studioSettleSQL, call, `{"eligible":true}`))
	if got := f.scalar(`SELECT count(*)::text FROM eacp.studio_node_results WHERE output IS NOT NULL`); got != "0" {
		t.Fatal("killed call published output")
	}
	if got := f.scalar(`SELECT state||' '||failure_reason FROM eacp.studio_runs WHERE id=$1`, run); got != "FAILED killed" {
		t.Fatalf("containment=%s", got)
	}
	_, err = f.beat(run, gen)
	wantState(t, err, sqlForbidden)
	ok(t, f.Exec("opal", killSQL, "run", run, false))
	if got := f.scalar(`SELECT eacp.llm_call_killed($1)::text`, call); got != "true" {
		t.Fatal("clearing kill revived bound call")
	}
}
