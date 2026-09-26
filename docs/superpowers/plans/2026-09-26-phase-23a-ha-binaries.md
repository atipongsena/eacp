# Phase 23a — High availability inside the binaries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make and prove the EACP binaries correct, cheap and graceful as N replicas: per-tenant loop locks for the evaluators, exact sweeper counters, a drain delay on shutdown, and a multi-replica integration test.

**Architecture:** No leader. PostgreSQL stays the only authority; a transaction-scoped `pg_try_advisory_xact_lock` per loop and tenant only avoids duplicate evaluator work. The sweeper keeps its row-lock arbitration and counts only real moves. `service.Deps.ServeOn` flips readiness to `draining`, stops background loops and keeps serving for `EACP_SHUTDOWN_DELAY` before the graceful shutdown.

**Tech Stack:** Go 1.27, pgx v5, PostgreSQL 18, existing `registrytest`/`pgtest`/`erpEnv` fixtures.

**Spec:** `docs/superpowers/specs/2026-09-26-ha-binaries-design.md`

## Global Constraints

- No migration, table, role or SECURITY DEFINER function (spec §2.5).
- No Kubernetes dependency in the binaries; no client-go (spec §2.2).
- Only transaction-scoped advisory locks (spec §2.3).
- `EACP_SHUTDOWN_DELAY` default `0s`, allowed `0s`–`60s` (spec §3.3).
- Loop names match `^[a-z][a-z0-9_]{0,31}$`; lock key `hashtextextended('eacp.loop:' || loop || ':' || tenant, 0)` (spec §3.1).
- Delivery wording per MASTER_PLAN §21: never "exactly-once".
- Tests run with `-race` and `EACP_TEST_ADMIN_DSN` set; commits as the user, no Co-Authored-By trailer.

## Review Focus

1. A loop lock taken outside a tenant transaction (no `app.tenant_id`) must error, never lock a NULL key or silently return false — pinned in Task 1 (`TestTryLoopLockNeedsATenant`).
2. An evaluator that skips a tenant must write nothing (no audit row, no incident) and the next pass must do the work — Task 2 skip tests.
3. A listener failure before any signal must return at once, not wait the drain delay — Task 4 (`TestListenerFailureReturnsWithoutDelay`).
4. Background loops must stop at the signal even when the delay is 0 — Task 4 (`TestNoDelayShutsDownAtOnce` checks the background context).
5. Stopping a replica mid-run must not leave an action un-terminal or produce a second ERP effect — Task 5.

---

### Task 1: `storage.TryLoopLock` and the fixture's lock holder

**Files:**
- Modify: `internal/storage/storage.go` (after `SetSystem`)
- Modify: `internal/registry/registrytest/registrytest.go` (new method at the end)
- Test: `internal/storage/looplock_test.go` (new)

**Interfaces:**
- Produces: `func TryLoopLock(ctx context.Context, tx pgx.Tx, loop string) (bool, error)`; `func (f *Fixture) HoldLoopLock(t testing.TB, loop string) (release func())`

- [ ] **Step 1: Write the failing tests** — `internal/storage/looplock_test.go`:

```go
package storage_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/storage"
)

// beginIn opens a transaction in tenant's context and returns it; the test
// ends it.
func beginIn(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, tenant string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if tenant != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
			t.Fatal(err)
		}
	}
	return tx
}

func tryLock(t *testing.T, tx pgx.Tx, loop string) bool {
	t.Helper()
	got, err := storage.TryLoopLock(context.Background(), tx, loop)
	if err != nil {
		t.Fatalf("TryLoopLock(%q) = %v", loop, err)
	}
	return got
}

func TestTryLoopLockIsPerLoopAndTenant(t *testing.T) {
	db := migratedDB(t)
	pool := appPool(t, db.AppDSN, 8)
	ctx := context.Background()
	holder := beginIn(t, pool, tenantA)
	if !tryLock(t, holder, "incident") {
		t.Fatal("first lock refused")
	}
	other := beginIn(t, pool, tenantA)
	if tryLock(t, other, "incident") {
		t.Fatal("the same loop and tenant was locked twice")
	}
	if !tryLock(t, other, "finops") {
		t.Fatal("another loop of the same tenant was refused")
	}
	if !tryLock(t, beginIn(t, pool, tenantB), "incident") {
		t.Fatal("the same loop of another tenant was refused")
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !tryLock(t, beginIn(t, pool, tenantA), "incident") {
		t.Fatal("the lock outlived its transaction")
	}
}

func TestTryLoopLockRejectsBadNames(t *testing.T) {
	db := migratedDB(t)
	tx := beginIn(t, appPool(t, db.AppDSN, 2), tenantA)
	for _, bad := range []string{"", "Incident", "a b", "1abc", "eacp.kill", "abcdefghijklmnopqrstuvwxyz0123456"} {
		if got, err := storage.TryLoopLock(context.Background(), tx, bad); err == nil || got {
			t.Errorf("TryLoopLock(%q) = %v, %v; want an error", bad, got, err)
		}
	}
}

func TestTryLoopLockNeedsATenant(t *testing.T) {
	db := migratedDB(t)
	tx := beginIn(t, appPool(t, db.AppDSN, 2), "")
	if got, err := storage.TryLoopLock(context.Background(), tx, "incident"); err == nil || got {
		t.Fatalf("TryLoopLock without a tenant = %v, %v; want an error", got, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race ./internal/storage -run TryLoopLock`
Expected: build failure `undefined: storage.TryLoopLock`.

- [ ] **Step 3: Implement** — in `internal/storage/storage.go` after `SetSystem` (add `"regexp"` to imports):

```go
// loopName is a background loop's name in its lock key.
var loopName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// TryLoopLock takes the transaction-scoped advisory lock of background loop
// loop for the transaction's tenant (set by InTenantTx) without waiting,
// and reports whether it got it. The lock only keeps replicas from doing the
// same tenant's work at once (ADR-029): it decides nothing, and it ends with
// the transaction, so a crashed replica never holds it.
func TryLoopLock(ctx context.Context, tx pgx.Tx, loop string) (bool, error) {
	if !loopName.MatchString(loop) {
		return false, fmt.Errorf("storage: invalid loop name %q", loop)
	}
	var got *bool
	err := tx.QueryRow(ctx, `SELECT CASE WHEN eacp.current_tenant_id() IS NULL THEN NULL
		ELSE pg_try_advisory_xact_lock(hashtextextended('eacp.loop:' || $1 || ':' || eacp.current_tenant_id()::text, 0)) END`,
		loop).Scan(&got)
	if err != nil {
		return false, fmt.Errorf("storage: loop lock: %w", err)
	}
	if got == nil {
		return false, errors.New("storage: loop lock needs a tenant transaction")
	}
	return *got, nil
}
```

(`errors` is already imported in storage.go; if not, add it.)

In `internal/registry/registrytest/registrytest.go` append:

```go
// HoldLoopLock takes background loop loop's lock for the fixture's tenant in
// an open transaction, as another replica evaluating the tenant would, and
// returns the function that ends that transaction.
func (f *Fixture) HoldLoopLock(t testing.TB, loop string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.App.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.Tenant.String()); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	got, err := storage.TryLoopLock(ctx, tx, loop)
	if err != nil || !got {
		_ = tx.Rollback(ctx)
		t.Fatalf("hold loop lock %s = %v, %v", loop, got, err)
	}
	return func() { _ = tx.Rollback(ctx) }
}
```

(Add `"eacp/internal/storage"` to the registrytest imports if it is not there.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/storage ./internal/registry/... -run 'TryLoopLock|TestFixture'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/storage internal/registry/registrytest
git commit -m "feat(storage): per-tenant loop lock for background loops (ADR-029)"
```

---

### Task 2: Evaluators skip a tenant another replica holds

**Files:**
- Modify: `internal/incident/evaluate.go` (`Evaluate`), `internal/finops/finops.go` (`Evaluate`), `internal/release/release.go` (`Evaluate`)
- Test: `internal/incident/ha_test.go`, `internal/finops/ha_test.go`, `internal/release/ha_test.go` (new)

**Interfaces:**
- Consumes: `storage.TryLoopLock`, `(*registrytest.Fixture).HoldLoopLock` (Task 1)
- Produces: loop names `incident`, `finops`, `release`; `Evaluate` returns `(0, nil)` when skipped.

- [ ] **Step 1: Write the failing tests**

`internal/incident/ha_test.go`:

```go
package incident_test

import (
	"context"
	"sync"
	"testing"

	"eacp/internal/incident"
	"eacp/internal/registry/registrytest"
)

func killedTool(t *testing.T, f *registrytest.Fixture) {
	t.Helper()
	tool := f.ActiveTool(t, "erp", "purchase")
	f.ActiveAgent(t, "buyer", tool.Tool)
	ok(t, f.Exec("otto", `SELECT eacp.set_kill('tool', $1, true, 'contain', 'security_incident')`, tool.Tool))
}

func TestIncidentEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	f := registrytest.New(t)
	killedTool(t, f)
	s := incident.New(f.App)
	release := f.HoldLoopLock(t, "incident")
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	if got := incidents(t, f, "kill"); len(got) != 0 {
		t.Fatalf("a skipped evaluation opened %d incidents", len(got))
	}
	release()
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

// Without the lock, PostgreSQL alone still opens one incident per signal:
// the lock saves work and decides nothing.
func TestConcurrentIncidentEvaluationsActOnce(t *testing.T) {
	f := registrytest.New(t)
	killedTool(t, f)
	s := incident.New(f.App)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { evaluate(t, f) }) // the SQL function, no loop lock
		wg.Go(func() {
			if _, err := s.Evaluate(context.Background(), f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := incidents(t, f, "kill"); len(got) != 1 {
		t.Fatalf("kill incidents = %d, want 1", len(got))
	}
}
```

`internal/finops/ha_test.go`:

```go
package finops_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"eacp/internal/finops"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func unpricedUsage(t *testing.T, f *registrytest.Fixture) {
	t.Helper()
	a := f.ActiveAgent(t, "buyer")
	if _, err := finops.New(f.App).RecordSpans(context.Background(), f.Tenant, a.Version,
		[]finops.Span{chatSpan("mystery", 5, 5, time.Now())}); err != nil {
		t.Fatal(err)
	}
}

func alertCount(t *testing.T, f *registrytest.Fixture) int {
	t.Helper()
	var n int
	if err := storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.finops_alerts`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFinOpsEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	f := registrytest.New(t)
	unpricedUsage(t, f)
	s := finops.New(f.App)
	release := f.HoldLoopLock(t, "finops")
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	if n := alertCount(t, f); n != 0 {
		t.Fatalf("a skipped evaluation raised %d alerts", n)
	}
	release()
	if n, err := s.Evaluate(context.Background(), f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

func TestConcurrentFinOpsEvaluationsActOnce(t *testing.T) {
	f := registrytest.New(t)
	unpricedUsage(t, f)
	s := finops.New(f.App)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := f.ExecSystem(finops.SystemActor, `SELECT eacp.finops_evaluate()`); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := s.Evaluate(context.Background(), f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := alertCount(t, f); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}
```

`internal/release/ha_test.go`:

```go
package release_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/release"
)

// breached returns a service whose tenant has one CANARY release with a
// guardrail breach, and that release's id.
func breached(t *testing.T) (svc, uuid.UUID) {
	t.Helper()
	v := newSvc(t)
	id := v.canary(t, cohort(t, v.world), plan{canaryActions: 2})
	v.f.QueuedAction(t, v.world.candidate, "carol", "erp.purchase")
	ok(t, v.f.ExecAgent(v.world.candidate, denySQL, v.f.ReceivedAction(t, v.world.candidate, "carol", "erp.purchase")))
	return v, id
}

func TestReleaseEvaluationSkipsATenantAnotherReplicaHolds(t *testing.T) {
	v, _ := breached(t)
	ctx := context.Background()
	hold := v.f.HoldLoopLock(t, "release")
	if n, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil || n != 0 {
		t.Fatalf("evaluate while held = %d, %v; want a skip", n, err)
	}
	hold()
	if n, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil || n != 1 {
		t.Fatalf("evaluate after release = %d, %v", n, err)
	}
}

// release_evaluate locks each CANARY release FOR UPDATE, so a second
// evaluation finds it ROLLED_BACK and does nothing; a double move would fail
// the state guard and surface as an error.
func TestConcurrentReleaseEvaluationsRollBackOnce(t *testing.T) {
	v, id := breached(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := v.f.ExecSystem(release.SystemActor, `SELECT eacp.release_evaluate()`); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			if _, err := v.s.Evaluate(ctx, v.f.Tenant); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	d, err := v.s.Get(ctx, v.principal("audra"), id)
	if err != nil || d.State != "ROLLED_BACK" {
		t.Fatalf("release = %+v, %v", d.Release, err)
	}
}
```

(Executor: if `v.canary` returns a type other than `uuid.UUID`, adapt `breached`'s return type — a ruling.)

- [ ] **Step 2: Run to verify the skip tests fail**

Run: `go test -race ./internal/incident ./internal/finops ./internal/release -run 'SkipsATenant|ActOnce|RollBackOnce'`
Expected: the three `…SkipsATenantAnotherReplicaHolds` tests FAIL (`evaluate while held = 1, <nil>; want a skip`); the `Concurrent…` tests PASS (they pin PostgreSQL's own arbitration and stay green after the change).

- [ ] **Step 3: Implement** — in each `Evaluate`, right after `storage.SetSystem(...)` succeeds:

`internal/incident/evaluate.go`:

```go
// Evaluate runs eacp.incident_evaluate() for tenant as the incident system
// actor and returns the number of incidents it opened. It skips the tenant
// (0, nil) while another replica is evaluating it (ADR-029).
func (s *Service) Evaluate(ctx context.Context, tenant uuid.UUID) (int, error) {
	var n int
	err := storage.InTenantTx(ctx, s.pool, tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetSystem(ctx, tx, "incident"); err != nil {
			return err
		}
		if got, err := storage.TryLoopLock(ctx, tx, "incident"); err != nil || !got {
			return err
		}
		return tx.QueryRow(ctx, `SELECT eacp.incident_evaluate()`).Scan(&n)
	})
	return n, err
}
```

`internal/finops/finops.go` `Evaluate`: same shape with `SystemActor`, loop `"finops"` and `eacp.finops_evaluate()`; doc comment gains "It skips the tenant (0, nil) while another replica is evaluating it (ADR-029)."

`internal/release/release.go` `Evaluate`: same shape with `SystemActor`, loop `"release"` and `eacp.release_evaluate()`; same doc sentence.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/incident ./internal/finops ./internal/release`
Expected: PASS (whole packages, so the existing evaluator tests stay green).

- [ ] **Step 5: Commit**

```bash
git add internal/incident internal/finops internal/release
git commit -m "feat: evaluators skip a tenant another replica is evaluating (ADR-029)"
```

---

### Task 3: Sweeper counts only the actions it moved

**Files:**
- Modify: `internal/action/sweeper.go` (`Stats` doc, `sweepTenant`)
- Test: `internal/action/ha_test.go` (new)

**Interfaces:**
- Produces: `Stats.Expired/Reclaimed/Retried/Escalated` count moves only; `Advanced` counts attempts.

- [ ] **Step 1: Write the failing test** — `internal/action/ha_test.go`:

```go
package action_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/action"
)

// Two replicas' sweepers select the same overdue actions before either
// moves them (a blocker holds the rows). Each action expires once, and the
// counters add up to the actions, not to twice as many.
func TestTwoSweepersMoveEachActionOnceAndCountIt(t *testing.T) {
	v := newEnv(t, allowAll)
	ctx := context.Background()
	var ids []uuid.UUID
	for _, key := range []string{"a", "b", "c"} {
		s := v.submission(key)
		s.Lifetime = time.Second
		got, err := v.e.Submit(ctx, v.actor(), s)
		if err != nil || got.State != "QUEUED" {
			t.Fatalf("submit %s = %+v, %v", key, got, err)
		}
		ids = append(ids, got.ID)
	}
	time.Sleep(1100 * time.Millisecond)

	admin, err := pgx.Connect(ctx, v.f.DB.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	blocker, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT id FROM eacp.actions WHERE id = ANY($1) FOR UPDATE`, ids); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stats := make([]action.Stats, 2)
	for i := range stats {
		wg.Go(func() {
			s := action.NewSweeper(v.e)
			s.Grace = 0
			st, err := s.RunOnce(ctx)
			if err != nil {
				t.Error(err)
			}
			stats[i] = st
		})
	}
	// Both sweepers have selected the actions and wait on the first row.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := v.f.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			_ = blocker.Rollback(ctx)
			t.Fatalf("the sweepers never both waited (%d waiting)", waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for _, id := range ids {
		if st := v.get(id).State; st != "EXPIRED" {
			t.Errorf("%s = %s, want EXPIRED", id, st)
		}
	}
	if sum := stats[0].Expired + stats[1].Expired; sum != len(ids) {
		t.Fatalf("expired counters = %d + %d, want %d in total", stats[0].Expired, stats[1].Expired, len(ids))
	}
}
```

(If `v.f.Owner` cannot read `pg_stat_activity` rows of other roles, use the `admin` connection for the wait query instead — a ruling, not a stall.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test -race ./internal/action -run TestTwoSweepersMoveEachActionOnceAndCountIt`
Expected: FAIL with `expired counters = … want 3 in total` (the sum is 6: the second sweeper counts the actions the first moved).

- [ ] **Step 3: Implement** — in `internal/action/sweeper.go`:

`Stats` doc:

```go
// Stats counts what one pass did. Expired, Reclaimed, Retried and Escalated
// count only the actions this pass moved (another replica may move one
// first); Advanced counts advance attempts.
type Stats struct {
```

In each loop of `sweepTenant`, declare `moved := false` before `s.e.inTx`, set `moved = true` only after a successful move, and increment only when moved:

- overdue actions: replace `return expire(ctx, tx, r)` with
  ```go
  if err := expire(ctx, tx, r); err != nil {
  	return err
  }
  moved = true
  return nil
  ```
  and after the error check: `if moved { st.Expired++ }`.
- lapsed approvals: capture the tag:
  ```go
  tag, err := tx.Exec(ctx, `UPDATE eacp.approval_requests SET state = 'EXPIRED' ...`, *r.ApprovalRequestID)
  moved = err == nil && tag.RowsAffected() == 1
  return err
  ```
  and `if moved { st.Expired++ }`.
- lapsed leases: both `return move(...)` become `err = move(...); moved = err == nil; return err` (declare `var err error` is already in scope from `QueryRow`; reuse it), and `if moved { st.Reclaimed++ }`.
- unknown outcomes: set `moved` the same way on the final `move`; after the error check:
  ```go
  if moved && escalated {
  	st.Escalated++
  } else if moved {
  	st.Retried++
  }
  ```
- retries: same pattern, `if moved { st.Retried++ }`.
- `Advanced` is unchanged.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/action`
Expected: PASS (existing counter assertions in `execution_test.go` and `reconcile_test.go` stay green).

- [ ] **Step 5: Commit**

```bash
git add internal/action
git commit -m "fix(action): sweeper counts only the actions it moved"
```

---

### Task 4: Drain on shutdown

**Files:**
- Modify: `internal/config/config.go` (field, parse, `LogValue`)
- Modify: `internal/service/service.go` (`Deps.draining`, `readinessChecks`, `Serve` → `ServeOn`)
- Test: `internal/config/config_test.go` (`TestShutdownDelaySetting`), `internal/service/drain_test.go` (new)

**Interfaces:**
- Produces: `config.Config.ShutdownDelay time.Duration`; `func (d *Deps) ServeOn(ctx context.Context, ln net.Listener, h http.Handler) error`; readiness check name `draining`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestShutdownDelaySetting(t *testing.T) {
	load := func(v string) (Config, error) {
		m := map[string]string{"EACP_DATABASE_URL": "postgres://app@localhost/eacp"}
		if v != "" {
			m["EACP_SHUTDOWN_DELAY"] = v
		}
		return Load(env(m), Options{RequireDatabase: true})
	}
	if cfg, err := load(""); err != nil || cfg.ShutdownDelay != 0 {
		t.Fatalf("default = %v, %v; want 0", cfg.ShutdownDelay, err)
	}
	for v, want := range map[string]time.Duration{"0s": 0, "5s": 5 * time.Second, "60s": time.Minute} {
		if cfg, err := load(v); err != nil || cfg.ShutdownDelay != want {
			t.Errorf("%s = %v, %v; want %v", v, cfg.ShutdownDelay, err, want)
		}
	}
	for _, bad := range []string{"-1s", "61s", "soon", "5"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "EACP_SHUTDOWN_DELAY") {
			t.Errorf("EACP_SHUTDOWN_DELAY=%q: err = %v", bad, err)
		}
	}
}
```

(Add `"time"` to the test imports if missing.)

`internal/service/drain_test.go`:

```go
package service_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"eacp/internal/config"
	"eacp/internal/service"
)

type served struct {
	base    string
	cancel  context.CancelFunc
	done    chan error
	stopped chan struct{} // closed when the background context ends
}

func serveWith(t *testing.T, delay string) served {
	t.Helper()
	env := map[string]string{}
	if delay != "" {
		env["EACP_SHUTDOWN_DELAY"] = delay
	}
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "test", envFrom(env), config.Options{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	s := served{done: make(chan error, 1), stopped: make(chan struct{})}
	deps.Background(func(ctx context.Context) {
		<-ctx.Done()
		close(s.stopped)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://" + ln.Addr().String()
	var ctx context.Context
	ctx, s.cancel = context.WithCancel(context.Background())
	go func() { s.done <- deps.ServeOn(ctx, ln, deps.Handler(http.NewServeMux())) }()
	if code, _ := get(t, s.base+"/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d", code)
	}
	return s
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDrainReportsNotReadyAndKeepsServing(t *testing.T) {
	s := serveWith(t, "600ms")
	start := time.Now()
	s.cancel()
	select {
	case <-s.stopped:
	case <-time.After(time.Second):
		t.Fatal("background loops kept running after the signal")
	}
	code, body := get(t, s.base+"/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"draining":"fail"`) {
		t.Fatalf("/readyz while draining = %d %s", code, body)
	}
	if code, _ := get(t, s.base+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz while draining = %d", code)
	}
	select {
	case err := <-s.done:
		t.Fatalf("served only %s of the delay: %v", time.Since(start), err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 600*time.Millisecond {
		t.Fatalf("shut down after %s, before the delay", elapsed)
	}
	if code, _ := get(t, s.base+"/healthz"); code != 0 {
		t.Fatalf("still serving after shutdown: %d", code)
	}
}

func TestNoDelayShutsDownAtOnce(t *testing.T) {
	s := serveWith(t, "")
	start := time.Now()
	s.cancel()
	if err := <-s.done; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %s without a delay", elapsed)
	}
	select {
	case <-s.stopped:
	case <-time.After(time.Second):
		t.Fatal("background loops kept running")
	}
}

func TestListenerFailureReturnsWithoutDelay(t *testing.T) {
	var out bytes.Buffer
	deps, stop, err := service.Start(context.Background(), "test",
		envFrom(map[string]string{"EACP_SHUTDOWN_DELAY": "60s"}), config.Options{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve fails at once
	start := time.Now()
	err = deps.ServeOn(context.Background(), ln, deps.Handler(http.NewServeMux()))
	if err == nil || errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("ServeOn on a closed listener = %v after %s", err, time.Since(start))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test -race ./internal/config ./internal/service -run 'ShutdownDelay|Drain|NoDelay|ListenerFailure'`
Expected: build failures `cfg.ShutdownDelay undefined` and `deps.ServeOn undefined`.

- [ ] **Step 3: Implement**

`internal/config/config.go`: next to `ShutdownTimeout` add

```go
	// ShutdownDelay is how long a stopping service keeps serving, reporting
	// not-ready, before its graceful HTTP shutdown (EACP_SHUTDOWN_DELAY,
	// 0s-60s, default 0s), so a load balancer stops routing to it first.
	ShutdownDelay time.Duration
```

and after the `EACP_SHUTDOWN_TIMEOUT` block:

```go
	delay, err := time.ParseDuration(get("EACP_SHUTDOWN_DELAY", "0s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("EACP_SHUTDOWN_DELAY: %w", err))
	case delay < 0 || delay > time.Minute:
		errs = append(errs, errors.New("EACP_SHUTDOWN_DELAY: must be between 0s and 60s"))
	}
	cfg.ShutdownDelay = delay
```

and in `LogValue`, beside the shutdown timeout attribute (or at the end if there is none): `slog.Duration("shutdown_delay", c.ShutdownDelay),`.

`internal/service/service.go`: add `"sync/atomic"`; in `Deps` add

```go
	// draining is set when shutdown starts; /readyz then fails.
	draining atomic.Bool
```

replace `readinessChecks`:

```go
// errDraining fails readiness once shutdown has started.
var errDraining = errors.New("service is shutting down")

func (d *Deps) readinessChecks() []health.Check {
	checks := []health.Check{{Name: "draining", Fn: func(context.Context) error {
		if d.draining.Load() {
			return errDraining
		}
		return nil
	}}}
	if d.DB == nil {
		return checks
	}
	return append(checks,
		health.Check{Name: "database", Fn: d.DB.Ping},
		health.Check{Name: "role_safety", Fn: func(ctx context.Context) error { return storage.CheckRoleSafety(ctx, d.DB) }},
		health.Check{Name: "schema_version", Fn: func(ctx context.Context) error {
			return storage.CheckSchemaVersion(ctx, d.DB, migrations.Latest())
		}},
	)
}
```

replace `Serve` with `Serve` + `ServeOn`:

```go
// Serve listens on the configured address and serves h until ctx is
// cancelled (see ServeOn).
func (d *Deps) Serve(ctx context.Context, h http.Handler) error {
	ln, err := net.Listen("tcp", d.Config.HTTPAddr)
	if err != nil {
		return fmt.Errorf("%s: listen: %w", d.Name, err)
	}
	return d.ServeOn(ctx, ln, h)
}

// ServeOn serves h on ln until ctx is cancelled, then drains (ADR-029):
// /readyz fails at once, background tasks are cancelled, the listener keeps
// serving for EACP_SHUTDOWN_DELAY so a load balancer can stop routing here,
// and the graceful shutdown then waits up to EACP_SHUTDOWN_TIMEOUT for
// in-flight requests. A listener failure returns at once.
func (d *Deps) ServeOn(ctx context.Context, ln net.Listener, h http.Handler) error {
	d.Log.Info("http listening", "addr", ln.Addr().String())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(d.Log.Handler(), slog.LevelWarn),
	}
	serving, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()
	go func() {
		select {
		case <-ctx.Done():
		case <-serving.Done(): // the server failed first
			return
		}
		d.draining.Store(true)
		d.bgCancel()
		if delay := d.Config.ShutdownDelay; delay > 0 {
			d.Log.Info("draining: not ready, still serving", "delay", delay)
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-serving.Done():
				t.Stop()
			}
		}
		stopServing()
	}()
	err := httpserver.Serve(serving, srv, ln, d.Config.ShutdownTimeout)
	if err == nil {
		d.Log.Info("http stopped cleanly")
	} else if errors.Is(err, context.DeadlineExceeded) {
		d.Log.Warn("http shutdown timed out; remaining connections closed")
	}
	return err
}
```

(`d.bgCancel` is idempotent; `stop` still cancels and waits for the background tasks.)

- [ ] **Step 4: Run to verify they pass**

Run: `go test -race ./internal/config ./internal/service ./internal/httpserver ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/service
git commit -m "feat(service): drain on shutdown - not ready, stop loops, keep serving for EACP_SHUTDOWN_DELAY"
```

---

### Task 5: Multi-replica proof

**Files:**
- Test: `internal/worker/ha_integration_test.go` (new)

**Interfaces:**
- Consumes: `erpEnv`, `newERPEnv`, `(*erpEnv).settled`, `(*erpEnv).effects`, `(*erpEnv).get` (existing); Tasks 2–3 behaviour.

- [ ] **Step 1: Write the test** — `internal/worker/ha_integration_test.go`:

```go
package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/action"
	"eacp/internal/audit"
	"eacp/internal/connector"
	"eacp/internal/finops"
	"eacp/internal/governance"
	"eacp/internal/incident"
	"eacp/internal/messaging"
	"eacp/internal/release"
	"eacp/internal/storage"
	"eacp/internal/worker"
)

// syncBuffer is a log sink shared by goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// replica is one process's loops on its own pool; stop cancels them as
// SIGTERM does, waits for them, then closes the pool.
type replica struct {
	engine *action.Engine
	stop   func()
}

func (v *erpEnv) pool() *pgxpool.Pool {
	v.t.Helper()
	p, err := pgxpool.New(context.Background(), v.f.DB.AppDSN)
	if err != nil {
		v.t.Fatal(err)
	}
	return p
}

func start(pool *pgxpool.Pool, loops ...func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, fn := range loops {
		wg.Go(func() { fn(ctx) })
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			pool.Close()
		})
	}
}

func (v *erpEnv) apiReplica(n int, log *slog.Logger) replica {
	pool := v.pool()
	provider := governance.LocalProvider{InstanceID: fmt.Sprintf("api-%d", n)}
	e := action.New(pool, action.Options{Provider: provider, Log: log})
	s := action.NewSweeper(e)
	s.Grace = 0
	rel := release.New(pool, release.Options{Provider: provider, Log: log})
	return replica{engine: e, stop: start(pool,
		func(ctx context.Context) { s.Run(ctx, 100*time.Millisecond) },
		func(ctx context.Context) { incident.New(pool).Run(ctx, 100*time.Millisecond, log) },
		func(ctx context.Context) { finops.New(pool).Run(ctx, 100*time.Millisecond, log) },
		func(ctx context.Context) { rel.Run(ctx, 100*time.Millisecond) },
		func(ctx context.Context) { messaging.RunPruner(ctx, pool, 100*time.Millisecond, log) },
	)}
}

func (v *erpEnv) workerReplica(n int, log *slog.Logger) replica {
	v.t.Helper()
	pool := v.pool()
	conns := map[string]worker.Connector{"http": connector.NewHTTP()}
	w, err := worker.New(pool, worker.Options{ID: fmt.Sprintf("w-ha-%d", n), Lease: 5 * time.Second,
		PollInterval: 50 * time.Millisecond, Connectors: conns, Secrets: v.secrets, Log: log,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	r, err := worker.NewReconciler(pool, worker.ReconcilerOptions{ID: fmt.Sprintf("r-ha-%d", n), Lease: 5 * time.Second,
		PollInterval: 50 * time.Millisecond, Connectors: conns, Secrets: v.secrets, Log: log,
		Backoff: func(int) time.Duration { return 100 * time.Millisecond }})
	if err != nil {
		v.t.Fatal(err)
	}
	return replica{stop: start(pool, w.Run, r.Run)}
}

// Three API replicas (sweeper, incident, FinOps and release evaluators,
// outbox pruner) and three workers (worker and reconciler) share one
// database. One of each stops a third of the way through. Every action
// still ends SUCCEEDED with one ERP record per operation key (effectively
// once where reconcilable), a standing kill opens one incident although
// three evaluators saw it, no loop reports a failure, the audit chain
// verifies, and more than one worker did the work.
func TestReplicasShareTheWorkAndSurviveLosingOne(t *testing.T) {
	v := newERPEnv(t)
	ctx := context.Background()
	idle := v.f.ActiveAgent(t, "idle")
	if err := v.f.Exec("otto", `SELECT eacp.set_kill('agent', $1, true, 'ha test', 'security_incident')`, idle.Agent); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	var apis, workers []replica
	for n := range 3 {
		apis = append(apis, v.apiReplica(n, log))
		workers = append(workers, v.workerReplica(n, log))
	}
	defer func() {
		for _, r := range append(apis, workers...) {
			r.stop()
		}
	}()

	scenarios := []map[string]any{
		{}, {"scenario": "execute_then_timeout", "delay_ms": 300}, {"scenario": "execute_then_reset"},
		{"scenario": "5xx_after_effect"}, {"scenario": "slow_response", "delay_ms": 50},
	}
	actor := action.Agent(v.f.Tenant, v.agent.Agent, v.agent.Version)
	var ids []uuid.UUID
	keys := map[uuid.UUID]string{}
	for i := range 30 {
		if i == 10 {
			apis[0].stop()
			workers[0].stop()
		}
		payload := map[string]any{"amount": 42, "currency": "THB"}
		for k, val := range scenarios[i%len(scenarios)] {
			payload[k] = val
		}
		b, _ := json.Marshal(payload)
		via := apis[i%3]
		if i >= 10 {
			via = apis[1+i%2]
		}
		a, err := via.engine.Submit(ctx, actor, action.Submission{IdempotencyKey: uuid.NewString(),
			Subject: "carol@tenant-a.test", Operation: "post", Target: "erp", Tool: "erp.create_po",
			ToolSchemaVersion: "1", Resource: "po", Payload: b})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		ids = append(ids, a.ID)
		keys[a.ID] = a.OperationKey
		time.Sleep(30 * time.Millisecond)
	}

	for _, got := range v.settled(ids, 2*time.Minute) {
		if got.State != "SUCCEEDED" || got.ExternalReference != "PO-"+got.ID.String() {
			t.Errorf("%s = %s (%s)", got.ID, got.State, got.StateReason)
		}
		if n := v.effects(keys[got.ID]); n != 1 {
			t.Errorf("%s: %d ERP records", got.ID, n)
		}
	}
	// Let every evaluator run a few more passes over the standing kill.
	time.Sleep(500 * time.Millisecond)

	var kills, workersUsed int
	err := storage.InTenantTx(ctx, v.f.Owner, v.f.Tenant.String(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.incidents WHERE kind = 'kill'`).Scan(&kills); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(DISTINCT worker_id) FROM eacp.actions WHERE id = ANY($1)`, ids).Scan(&workersUsed)
	})
	if err != nil {
		t.Fatal(err)
	}
	if kills != 1 {
		t.Errorf("kill incidents = %d, want 1", kills)
	}
	if workersUsed < 2 {
		t.Errorf("%d worker ids executed the actions, want the work shared", workersUsed)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"level":"ERROR"`) {
			t.Errorf("a replica logged an error: %s", line)
		}
	}
	if err := storage.InTenantReadTx(ctx, v.f.App, v.f.Tenant.String(), func(tx pgx.Tx) error {
		_, err := audit.Verify(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -race ./internal/worker -run TestReplicasShareTheWorkAndSurviveLosingOne -count=1 -v`
Expected: PASS. This test proves existing and Tasks 2–3 behaviour; it has no RED step of its own (ledger that). If it fails, the failure is a finding: debug it with superpowers:systematic-debugging (a real HA defect is fixed test-first in the owning package; a harness defect is fixed here). Known adjustments that are rulings, not stalls: `set_kill` scope name for agents (check `eacp.set_kill`'s accepted scopes in `migrations/00016*.sql`), `worker.Options`/`ReconcilerOptions` field names (`Log`), and whether the worker's lease-expiry log lines are WARN, not ERROR, in a clean run.

- [ ] **Step 3: Commit**

```bash
git add internal/worker/ha_integration_test.go
git commit -m "test(worker): three API replicas and three workers share one database and survive losing one"
```

---

### Task 6: ADR-029 and the docs

**Files:**
- Create: `docs/adr/ADR-029-high-availability.md`
- Modify: `docs/adr/README.md`, `docs/MASTER_PLAN.md` §95, `AGENTS.md`, `README.md`, `docs/INVARIANTS.md` (only if `test/invariants` accepts the new test as extra evidence; never remove a listed test)

- [ ] **Step 1: Write ADR-029** with: Status (Accepted, Rev 1.0, 2026-09-26, Phase 23a; 23b extends it), Context (§95, what already holds, the four gaps from spec §1), Decision §1 no leader + loop lock (key, loop names, skip semantics, pooler-safety), §2 sweeper/pruner row-lock arbitration and exact counters, §3 drain sequence and `EACP_SHUTDOWN_DELAY`, §4 the proof test and the per-package tests, Consequences (evaluator work per interval is one per tenant; a skipped tenant waits at most one interval; compose unchanged; Kubernetes needs `terminationGracePeriodSeconds` ≥ delay + shutdown timeout + the longest connector call — for 23b), and the unresolved-assumptions table copied from spec §5.
- [ ] **Step 2: ADR index row** in `docs/adr/README.md` following the ADR-028 row's format.
- [ ] **Step 3: MASTER_PLAN §95** — add a status line under the heading like §94's: "Status: 23a delivered (ADR-029): no leader, per-tenant loop locks, drain on shutdown, multi-replica proof; 23b (Helm, NetworkPolicies, PDBs, autoscaling, minikube) next."
- [ ] **Step 4: AGENTS.md** — status sentence gains "Phase 23a (HA inside the binaries, ADR-029) is complete; 23b (Kubernetes) is next." and a rule bullet: "Replicas share nothing that decides (ADR-029). No leader election: a background loop that evaluates per tenant takes `storage.TryLoopLock` in its tenant transaction and skips the tenant when it is held; a loop that moves rows relies on row locks and compare-and-set and counts only real moves. The lock never decides; keep a test showing the outcome is the same without it. Shutdown fails `/readyz` (`draining`), cancels background loops and serves for `EACP_SHUTDOWN_DELAY` before the graceful stop."
- [ ] **Step 5: README** — a "Phase 23a: running several replicas" paragraph (what is safe to scale, the setting, the proof test).
- [ ] **Step 6: Verify and commit**

Run: `go vet ./... && go test -race ./test/invariants ./internal/storage ./internal/service`
Expected: PASS.

```bash
git add docs AGENTS.md README.md
git commit -m "docs: ADR-029 high availability; Phase 23a delivered"
```
