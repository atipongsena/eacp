package worker_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
	"eacp/internal/worker"
)

func schedulerCandidates(t *testing.T, f *registrytest.Fixture, limit int) []worker.Candidate {
	t.Helper()
	store := worker.NewStore(f.App, "scheduler-test")
	got, err := store.Claimable(context.Background(), []string{"http"}, []worker.Binding{{TenantID: uuid.MustParse(pgtest.TenantA), Ref: "erp", Host: "fakeerp:8090"}}, limit)
	must(t, err)
	return got
}

func schedulerGroupAgent(t *testing.T, f *registrytest.Fixture, name string, weight int, tool uuid.UUID) registrytest.Agent {
	t.Helper()
	group := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
		VALUES (eacp.current_tenant_id(), $1, $1, $2) RETURNING id`, name, weight)
	a := registrytest.Agent{}
	a.Agent = f.ID(t, "erin", `INSERT INTO eacp.agents
		(tenant_id, name, display_name, environment, risk_class, owner_group_id)
		VALUES (eacp.current_tenant_id(), $1, $1, 'production', 'high', $2) RETURNING id`, name, group)
	a.Version = f.ID(t, "erin", `INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, 'python', 'git:abc123') RETURNING id`, a.Agent)
	allow := f.ID(t, "erin", `INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, a.Version, []uuid.UUID{tool})
	must(t, f.Exec("rita", `UPDATE eacp.agent_versions SET active_allowlist_id = $1 WHERE id = $2`, allow, a.Version))
	must(t, f.Exec("ravi", `UPDATE eacp.agent_versions SET state = 'ACTIVE', state_reason = 'go live' WHERE id = $1`, a.Version))
	return a
}

func TestSchedulerServesSmallTeamDespiteOlderBacklog(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "lookup")
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	a := f.ActiveAgent(t, "heavy", tool.Tool)
	b := f.ActiveAgent(t, "small", tool.Tool)
	for i := 0; i < 12; i++ {
		f.QueuedAction(t, a.Version, "carol", "erp.lookup")
	}
	small := f.QueuedAction(t, b.Version, "carol", "erp.lookup")
	store := worker.NewStore(f.App, "scheduler-test")
	ctx := context.Background()
	seen := false
	for i := 0; i < 3; i++ {
		candidates := schedulerCandidates(t, f, 20)
		if len(candidates) == 0 {
			t.Fatal("scheduler returned no eligible action")
		}
		if candidates[0].ActionID == small {
			seen = true
			break
		}
		_, ok, err := store.Claim(ctx, candidates[0], time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("uncontended claim failed")
		}
	}
	if !seen {
		t.Fatal("small team's action did not reach the head within three claims")
	}
}

func TestSchedulerUsesPinnedTeamWeight(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "lookup")
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	heavy := schedulerGroupAgent(t, f, "weighted", 3, tool.Tool)
	light := schedulerGroupAgent(t, f, "unweighted", 1, tool.Tool)
	heavyIDs := make(map[uuid.UUID]bool)
	for i := 0; i < 10; i++ {
		heavyIDs[f.QueuedAction(t, heavy.Version, "carol", "erp.lookup")] = true
	}
	for i := 0; i < 10; i++ {
		f.QueuedAction(t, light.Version, "carol", "erp.lookup")
	}
	store := worker.NewStore(f.App, "scheduler-test")
	count := 0
	for i := 0; i < 8; i++ {
		c := schedulerCandidates(t, f, 20)[0]
		if heavyIDs[c.ActionID] {
			count++
		}
		_, ok, err := store.Claim(context.Background(), c, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("uncontended weighted claim failed")
		}
	}
	if count != 6 {
		t.Fatalf("weight-3 team received %d of first 8 claims, want 6", count)
	}
}

func TestSchedulerServesTenantWithSmallerBacklog(t *testing.T) {
	f := registrytest.New(t)
	other := f.ForTenant(t, pgtest.TenantB)
	aTool := f.ActiveTool(t, "erp", "lookup")
	bTool := other.ActiveTool(t, "erp", "lookup")
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	other.ActivatePolicy(t, registrytest.AllowPolicy)
	a := f.ActiveAgent(t, "heavy", aTool.Tool)
	b := other.ActiveAgent(t, "small", bTool.Tool)
	for i := 0; i < 8; i++ {
		f.QueuedAction(t, a.Version, "carol", "erp.lookup")
	}
	small := other.QueuedAction(t, b.Version, "carol", "erp.lookup")
	store := worker.NewStore(f.App, "scheduler-test")
	bindings := []worker.Binding{
		{TenantID: uuid.MustParse(pgtest.TenantA), Ref: "erp", Host: "fakeerp:8090"},
		{TenantID: uuid.MustParse(pgtest.TenantB), Ref: "erp", Host: "fakeerp:8090"},
	}
	seen := false
	for i := 0; i < 2; i++ {
		got, err := store.Claimable(context.Background(), []string{"http"}, bindings, 20)
		must(t, err)
		if got[0].ActionID == small {
			seen = true
			break
		}
		_, ok, err := store.Claim(context.Background(), got[0], time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("uncontended tenant claim failed")
		}
	}
	if !seen {
		t.Fatal("tenant B did not reach the head within two claims")
	}
}

func TestConnectorCapacityIsEnforcedForRawClaims(t *testing.T) {
	f := registrytest.New(t)
	const capacityContract = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, max_inflight)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 1)
		RETURNING id`
	tool := f.ActiveToolWith(t, "erp", "lookup", capacityContract)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	first := f.QueuedAction(t, agent.Version, "carol", "erp.lookup")
	second := f.QueuedAction(t, agent.Version, "carol", "erp.lookup")
	must(t, f.ExecWorker("w1", 1, claimSQL, first, "w1", 60))
	wantCode(t, f.ExecWorker("w2", 1, claimSQL, second, "w2", 60), "53300")
	for _, c := range schedulerCandidates(t, f, 10) {
		if c.ActionID == second {
			t.Fatal("capacity-saturated connector was advertised as claimable")
		}
	}
	must(t, f.ExecWorker("w1", 1, `UPDATE eacp.actions SET state = 'QUEUED', state_reason = 'released' WHERE id = $1`, first))
	// Retrying the same queued action with capacity available is permitted.
	must(t, f.ExecWorker("w2", 1, claimSQL, second, "w2", 60))
}

func TestConnectorCapacitySerializesConcurrentClaims(t *testing.T) {
	f := registrytest.New(t)
	const contract = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, max_inflight)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 1)
		RETURNING id`
	tool := f.ActiveToolWith(t, "erp", "lookup", contract)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	ids := []uuid.UUID{f.QueuedAction(t, agent.Version, "carol", "erp.lookup"),
		f.QueuedAction(t, agent.Version, "carol", "erp.lookup")}
	start := make(chan struct{})
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id uuid.UUID) {
			defer wg.Done()
			<-start
			_, ok, err := worker.NewStore(f.App, []string{"w1", "w2"}[i]).Claim(context.Background(),
				worker.Candidate{TenantID: uuid.MustParse(pgtest.TenantA), ActionID: id}, time.Minute)
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
			}
			results <- ok
		}(i, id)
	}
	close(start)
	wg.Wait()
	close(results)
	claimed := 0
	for ok := range results {
		if ok {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claims = %d, want one", claimed)
	}
}

func TestConnectorCapacityRejectsStaleRepeatableReadClaim(t *testing.T) {
	f := registrytest.New(t)
	const contract = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, max_inflight)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 1)
		RETURNING id`
	tool := f.ActiveToolWith(t, "erp", "lookup", contract)
	agent := f.ActiveAgent(t, "buyer", tool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	first := f.QueuedAction(t, agent.Version, "carol", "erp.lookup")
	second := f.QueuedAction(t, agent.Version, "carol", "erp.lookup")
	ctx := context.Background()
	tx, err := f.App.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	must(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, pgtest.TenantA)
	must(t, err)
	must(t, storage.SetWorker(ctx, tx, "w2", 1))
	var count int
	must(t, tx.QueryRow(ctx, `SELECT count(*) FROM eacp.actions WHERE state = 'LEASED'`).Scan(&count))
	if count != 0 {
		t.Fatalf("initial active count = %d, want zero", count)
	}
	must(t, f.ExecWorker("w1", 1, claimSQL, first, "w1", 60))
	_, err = tx.Exec(ctx, claimSQL, second, "w2", 60)
	if err == nil {
		err = tx.Commit(ctx)
	}
	// A stale snapshot cannot commit another lease. The turn-row conflict
	// raises 40001; a capacity check after a fresh snapshot raises 53300.
	if err == nil {
		t.Fatal("repeatable-read snapshot exceeded connector capacity")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "40001" && pgErr.Code != "53300") {
		t.Fatalf("stale snapshot error = %v, want 40001 or 53300", err)
	}
}

func TestNamedCapacityGroupSpansConnectorsAndStateIsTenantIsolated(t *testing.T) {
	f := registrytest.New(t)
	const contract = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, concurrency_group, max_inflight)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 'shared', 1)
		RETURNING id`
	a := f.ActiveToolWith(t, "erp", "lookup", contract)
	b := f.ActiveToolWith(t, "other", "lookup", contract)
	agent := f.ActiveAgent(t, "buyer", a.Tool, b.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	first := f.QueuedAction(t, agent.Version, "carol", "erp.lookup")
	second := f.QueuedAction(t, agent.Version, "carol", "other.lookup")
	must(t, f.ExecWorker("w1", 1, claimSQL, first, "w1", 60))
	wantCode(t, f.ExecWorker("w2", 1, claimSQL, second, "w2", 60), "53300")
	wantCode(t, f.Exec("alice", `UPDATE eacp.scheduler_team_state SET credit = 9`), "42501")
	ctx := context.Background()
	var count int
	must(t, storage.InTenantTx(ctx, f.App, pgtest.TenantB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM eacp.scheduler_team_state`).Scan(&count)
	}))
	if count != 0 {
		t.Fatalf("tenant B sees %d scheduler rows from tenant A", count)
	}
}

func TestSchedulerPriorityAndAging(t *testing.T) {
	f := registrytest.New(t)
	lowTool := f.ActiveTool(t, "erp", "low")
	const highContract = `INSERT INTO eacp.tool_contracts
		(tenant_id, tool_id, side_effects, idempotency_mode, reconciliation_lookup,
		 reconciliation_consistency, proof_standard, max_attempts, schedule_priority)
		VALUES (eacp.current_tenant_id(), $1, '{READ_ONLY}', 'none', 'none', 'none', 'none', 3, 8)
		RETURNING id`
	highTool := f.ActiveToolWith(t, "priority", "high", highContract)
	agent := f.ActiveAgent(t, "buyer", lowTool.Tool, highTool.Tool)
	f.ActivatePolicy(t, registrytest.AllowPolicy)
	low := f.QueuedAction(t, agent.Version, "carol", "erp.low")
	high := f.QueuedAction(t, agent.Version, "carol", "priority.high")
	// Both bindings belong to this worker.
	store := worker.NewStore(f.App, "scheduler-test")
	bindings := []worker.Binding{
		{TenantID: uuid.MustParse(pgtest.TenantA), Ref: "erp", Host: "fakeerp:8090"},
		{TenantID: uuid.MustParse(pgtest.TenantA), Ref: "priority", Host: "fakeerp:8090"},
	}
	query := func() uuid.UUID {
		got, err := store.Claimable(context.Background(), []string{"http"}, bindings, 10)
		must(t, err)
		return got[0].ActionID
	}
	if got := query(); got != high {
		t.Fatalf("priority chose %s, want %s", got, high)
	}
	// A near deadline action reaches the priority ceiling immediately.
	ctx := context.Background()
	editLow := func(sql string) {
		t.Helper()
		must(t, storage.InTenantTx(ctx, f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE eacp.actions DISABLE TRIGGER actions_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, sql, low); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `ALTER TABLE eacp.actions ENABLE TRIGGER actions_guard`)
			return err
		}))
	}
	editLow(`UPDATE eacp.actions SET not_after = now() + interval '4 minutes' WHERE id = $1`)
	if got := query(); got != low {
		t.Fatalf("near-deadline action chose %s, want %s", got, low)
	}
	// Aging reaches the priority ceiling after 45 minutes; FIFO then wins.
	editLow(`UPDATE eacp.actions SET not_after = now() + interval '1 hour',
		state_changed_at = now() - interval '45 minutes' WHERE id = $1`)
	if got := query(); got != low {
		t.Fatalf("aged action chose %s, want %s", got, low)
	}
}

// BenchmarkSchedulerFairness exercises the plan's 10,000:100:100 backlog.
// Run with -run '^$' -bench BenchmarkSchedulerFairness -benchtime=1x.
func BenchmarkSchedulerFairness(b *testing.B) {
	f := registrytest.New(b)
	tool := f.ActiveTool(b, "erp", "lookup")
	f.ActivatePolicy(b, registrytest.AllowPolicy)
	agents := []registrytest.Agent{
		f.ActiveAgent(b, "team-a", tool.Tool),
		f.ActiveAgent(b, "team-b", tool.Tool),
		f.ActiveAgent(b, "team-c", tool.Tool),
	}
	ctx := context.Background()
	var cols []string
	rows, err := f.Owner.Query(ctx, `SELECT attname FROM pg_attribute
		WHERE attrelid = 'eacp.actions'::regclass AND attnum > 0 AND NOT attisdropped AND attgenerated = ''
		ORDER BY attnum`)
	if err != nil {
		b.Fatal(err)
	}
	cols, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		b.Fatal(err)
	}
	projection := make([]string, len(cols))
	for i, col := range cols {
		switch col {
		case "id":
			projection[i] = "gen_random_uuid()"
		case "idempotency_key":
			projection[i] = "'bench-' || $3 || '-' || g::text"
		default:
			projection[i] = "template." + pgx.Identifier{col}.Sanitize()
		}
		cols[i] = pgx.Identifier{col}.Sanitize()
	}
	bulkSQL := fmt.Sprintf(`INSERT INTO eacp.actions (%s)
		SELECT %s FROM eacp.actions template CROSS JOIN generate_series(1, $2) g
		WHERE template.id = $1`, strings.Join(cols, ", "), strings.Join(projection, ", "))
	seeds := make([]uuid.UUID, len(agents))
	for i, agent := range agents {
		seeds[i] = f.QueuedAction(b, agent.Version, "carol", "erp.lookup")
	}
	err = storage.InTenantTx(ctx, f.Owner, pgtest.TenantA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE eacp.actions DISABLE TRIGGER USER`); err != nil {
			return err
		}
		for i, seed := range seeds {
			count := 99
			if i == 0 {
				count = 9999
			}
			if _, err := tx.Exec(ctx, bulkSQL, seed, count, strconv.Itoa(i)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `ALTER TABLE eacp.actions ENABLE TRIGGER USER`)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	store := worker.NewStore(f.App, "benchmark")
	bindings := []worker.Binding{{TenantID: uuid.MustParse(pgtest.TenantA), Ref: "erp", Host: "fakeerp:8090"}}
	counts := [3]int{}
	b.ResetTimer()
	for range b.N {
		for i := 0; i < 30; i++ {
			candidates, err := store.Claimable(ctx, []string{"http"}, bindings, 1)
			if err != nil || len(candidates) != 1 {
				b.Fatalf("claim hint = %v, %v", candidates, err)
			}
			var agentID uuid.UUID
			err = storage.InTenantTx(ctx, f.App, pgtest.TenantA, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT agent_id FROM eacp.actions WHERE id = $1`, candidates[0].ActionID).Scan(&agentID)
			})
			if err != nil {
				b.Fatal(err)
			}
			for j, agent := range agents {
				if agent.Agent == agentID {
					counts[j]++
				}
			}
			if _, ok, err := store.Claim(ctx, candidates[0], time.Minute); err != nil || !ok {
				b.Fatalf("claim failed: %v, %v", ok, err)
			}
		}
	}
	b.StopTimer()
	if counts[1] < 5 || counts[2] < 5 {
		b.Fatalf("starvation with 10,000:100:100 backlog: %v", counts)
	}
	b.ReportMetric(float64(counts[1]+counts[2]), "small-team-claims")
}
