# Phase 20 — Governance-as-Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let registry owners declare connectors, tools, contracts, agents, versions and allowlists in a YAML bundle, plan it against a tenant, and apply it through a two-person change set recorded in PostgreSQL, with read-only drift detection.

**Architecture:** `eacpctl bundle` resolves a YAML bundle (targets, variables) into one JSON document. The API's planner (`internal/bundle`) diffs it against the registry in one REPEATABLE READ snapshot and records a change set: ordered steps, the objects it read (refs), and a digest that PostgreSQL computes. Submit runs the submit-stage steps as person A; approve runs the rest as person B ≠ A. Each step is the same SQL `registry.Service` runs (`registry.Tx`), so every existing registry trigger still decides it. Migration 00020 binds each stage to its transaction and actor, refuses a stale change set, and journals every move.

**Tech Stack:** Go 1.27, pgx v5, PostgreSQL (goose migrations, plpgsql triggers), `go.yaml.in/yaml/v3` v3.0.5 (already in `go.sum`; `Decoder.KnownFields` verified in the module cache), net/http.

**Spec:** `docs/superpowers/specs/2026-09-25-governance-as-code-design.md`

## Global Constraints

- ADR number: **ADR-026 Governance-as-Code**. Migration: **`migrations/00020_governance_as_code.sql`**.
- PostgreSQL computes every digest (`desired_digest`, `base_digest`, `sealed_digest`); Go never sends one.
- Every new table follows the RLS convention of `migrations/00001_foundation.sql` (ENABLE + FORCE + `tenant_isolation` policy) and is added to `internal/storage/rls_catalog_test.go`. No SECURITY DEFINER function is added.
- Every privileged write binds `storage.SetActor`; the change-set guards take the actor from `eacp.current_actor_id()`.
- Nothing is ever deleted: no DELETE grant is added, prune uses only `transition RETIRED` and `revoke` of a contract.
- A change set never activates a version while another version of the agent is `ACTIVE` (finding `requires_release`), and never undoes containment (finding `contained`).
- The desired document never carries a secret value: strict decoding refuses unknown fields; connectors carry only `secret_ref`.
- Agents' `environment` values are `development`, `staging`, `production`; `risk_class` values are `low`, `medium`, `high`, `critical`.
- MCP tools are discovered, never declared (ADR-023): a bundle may only reference an existing MCP tool and pin its current `definition_id`.
- Tests run with `EACP_TEST_ADMIN_DSN=postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable` (`docker compose up -d postgres` first) and `-race`. Without the DSN the database tests **skip**, which is not a pass.
- On Git Bash, prepend Go: `export PATH="/c/Program Files/Go/bin:$PATH"`.
- Commit as the user only. **No `Co-Authored-By` trailer.**
- Do not start Phase 21 after this plan. Stop and report (MASTER_PLAN §107).

## Review Focus

1. **Cosmetic YAML differences** (a contract number written `5`, `5.0` or `"5.000000"`, an allowlist in a different order) must plan **no** step. Test: `TestContractNumbersAndAllowlistOrderDoNotDrift` (Task 4).
2. **A huge bundle** must fail with a clear finding, not a database CHECK error. Test: `TestPlansAreBoundedTo2000Steps` (Task 4).
3. **Submitting the same change set twice, or approving one that is not SUBMITTED**, must return a conflict, not a partial run. Test: `TestSubmitAndApproveApplyTheBundle` resubmits and re-approves (Task 6).
4. **A principal of another tenant** must see no change set, bundle or drift of this tenant (404). Test: `TestChangeSetsOfOtherTenantsAreNotFound` (Task 8).
5. **A secret-looking field** such as `token` or `password` in the YAML must be refused, not silently dropped. Test: `TestDecodeRefusesUnknownFieldsSoNoSecretIsCarried` (Task 3).
6. **`eacpctl bundle drift` or `list` on a bundle whose variables have no value here** must still work: only plan, deploy and validate need the resolved document. Test: `TestBundleCommands` runs `list` and `drift` without `--var` (Task 9).

---

## File Structure

| File | Responsibility |
|---|---|
| `migrations/00020_governance_as_code.sql` (create) | Tables, guards, digest and seal functions, commit-time audit |
| `internal/storage/storage.go` (modify) | `InTenantSnapshotTx`: read-write REPEATABLE READ tenant transaction |
| `internal/storage/rls_catalog_test.go` (modify) | Review the five new tables |
| `internal/registry/tx.go` (create) | `registry.Tx`: tx-level registry writes shared by `Service` and change sets; `MapErr` |
| `internal/registry/agents.go`, `connectors.go` (modify) | Service methods call `registry.Tx` |
| `internal/bundle/document.go` (create) | Desired document types, strict `Decode`, `Validate`, findings |
| `internal/bundle/state.go` (create) | `State` and `loadState`: the registry as one plan sees it |
| `internal/bundle/plan.go` (create) | Pure `diff`: steps, findings and refs; contract normalization |
| `internal/bundle/service.go` (create) | `Service`: `Plan`, `Get`, `List`, `Bundles`; recording a change set; errors |
| `internal/bundle/execute.go` (create) | `Submit`, `Approve`, `Reject`, step execution |
| `internal/bundle/drift.go` (create) | `Drift` |
| `internal/bundle/*_test.go` (create) | Schema (raw SQL), unit (diff), integration (service) tests |
| `internal/api/bundle.go` (create), `api.go` (modify) | Routes, error mapping |
| `internal/api/bundle_test.go` (create) | API tests |
| `cmd/eacpctl/bundle.go` (create), `client.go`, `main.go` (modify) | `eacpctl bundle` commands, YAML loading |
| `cmd/eacpctl/bundle_test.go` (create) | CLI tests |
| `test/demo/slice_c_test.go`, `docs/DEMO.md` (modify) | Demo step |
| `docs/adr/ADR-026-governance-as-code.md` (create), `docs/adr/README.md`, `docs/MASTER_PLAN.md`, `docs/INVARIANTS.md`, `AGENTS.md` (modify) | Docs |

---

### Task 1: Migration 00020 and its schema tests

**Files:**
- Create: `migrations/00020_governance_as_code.sql`
- Create: `internal/bundle/schema_test.go`
- Create: `internal/bundle/doc.go` (package comment only, so the test package compiles)
- Modify: `internal/storage/rls_catalog_test.go` (after the `releases` check, around line 88)

**Interfaces:**
- Produces (SQL): tables `eacp.bundles`, `eacp.change_sets`, `eacp.change_set_steps`, `eacp.change_set_refs`, `eacp.bundle_resources`; functions `eacp.change_set_ref_row(text, uuid) → text`, `eacp.change_set_digest(uuid) → bytea`, `eacp.change_set_seal(uuid) → bytea`. SQLSTATEs: `42501` role/two-person, `55000` bad state, `23514` check, `40001` stale, `23505` one open change set per bundle.
- State moves the Go code will make: `PLANNED→SUBMITTED` (with approve steps), `PLANNED→APPLIED` (without), `SUBMITTED→APPLIED`, `PLANNED|SUBMITTED→REJECTED`, `PLANNED→SUPERSEDED`.

- [ ] **Step 1: Write the package comment**

`internal/bundle/doc.go`:

```go
// Package bundle plans and applies Governance-as-Code change sets
// (ADR-026).
//
// A bundle is a named desired state for part of the registry: connectors,
// their tools and contracts, agents, versions and allowlists. Plan diffs it
// against the registry in one snapshot and records a change set whose steps
// are the registry writes that would converge it. Submit runs the
// submit-stage steps as the submitter; a second person approves and runs
// the rest. PostgreSQL (migration 00020) binds each stage to its
// transaction and actor, computes every digest, refuses a stale change set
// and journals each move; the registry triggers still decide every step.
// Nothing is ever deleted, and nothing here grants more than the API does.
package bundle
```

- [ ] **Step 2: Write the failing schema tests**

`internal/bundle/schema_test.go`:

```go
package bundle_test

// Schema-level tests (ADR-026 §5): every change-set rule is enforced by
// PostgreSQL, so these tests issue raw SQL as the application role.

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
	"eacp/internal/storage/pgtest"
)

const (
	sqlForbidden = "42501"
	sqlBadState  = "55000"
	sqlCheck     = "23514"
	sqlStale     = "40001"
	sqlUnique    = "23505"
)

func wantState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %v", err, codes)
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return
		}
	}
	t.Fatalf("SQLSTATE %s (%s), want %v", pgErr.Code, pgErr.Message, codes)
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// inTx runs fn as actor in one transaction of the fixture tenant.
func inTx(f *registrytest.Fixture, actor string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := context.Background()
	return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

type rawStep struct{ address, op, stage string }
type rawRef struct {
	kind string
	id   uuid.UUID
}

// plan records and seals a PLANNED change set of bundle with steps and refs
// in one transaction as actor.
func plan(f *registrytest.Fixture, actor, bundle string, steps []rawStep, refs []rawRef) (uuid.UUID, error) {
	id := uuid.New()
	err := inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
		if err := planIn(ctx, tx, id, bundle, steps, refs); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	})
	return id, err
}

func planIn(ctx context.Context, tx pgx.Tx, id uuid.UUID, bundle string, steps []rawStep, refs []rawRef) error {
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.bundles (tenant_id, name) VALUES (eacp.current_tenant_id(), $1)
		ON CONFLICT (tenant_id, name) DO NOTHING`, bundle); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_sets (tenant_id, id, bundle_id, state, desired, prune)
		SELECT eacp.current_tenant_id(), $1, id, 'PLANNED', '{"test": true}', false
		FROM eacp.bundles WHERE name = $2`, id, bundle); err != nil {
		return err
	}
	for i, s := range steps {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_steps
			(tenant_id, change_set_id, ordinal, address, op, stage, payload)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, '{}')`, id, i+1, s.address, s.op, s.stage); err != nil {
			return err
		}
	}
	for _, r := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
			VALUES (eacp.current_tenant_id(), $1, $2, $3)`, id, r.kind, r.id); err != nil {
			return err
		}
	}
	return nil
}

// runSteps marks each listed step (by ordinal) as run with a fresh object id.
func runSteps(ctx context.Context, tx pgx.Tx, id uuid.UUID, ordinals ...int) error {
	for _, o := range ordinals {
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_set_steps SET object_id = $3
			WHERE change_set_id = $1 AND ordinal = $2`, id, o, uuid.New()); err != nil {
			return err
		}
	}
	return nil
}

func setState(ctx context.Context, tx pgx.Tx, id uuid.UUID, state string) error {
	_, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = $2 WHERE id = $1`, id, state)
	return err
}

func twoStage() []rawStep {
	return []rawStep{{"connector.ledger", "create", "submit"}, {"tool.ledger.post", "create", "submit"},
		{"contract.ledger.post", "activate", "approve"}}
}

func auditCount(t *testing.T, f *registrytest.Fixture, action string) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.audit_events
			WHERE convert_from(payload, 'UTF8')::jsonb->>'action' = $1`, action).Scan(&n)
	}))
	return n
}

func TestPlanningNeedsARegistryRoleAndIsSealed(t *testing.T) {
	f := registrytest.New(t)
	_, err := plan(f, "carol", "ledger", twoStage(), nil)
	wantState(t, err, sqlForbidden)
	_, err = plan(f, "otto", "ledger", twoStage(), nil)
	wantState(t, err, sqlForbidden)

	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	var desired, base []byte
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT desired_digest, base_digest FROM eacp.change_sets WHERE id = $1`, id).
			Scan(&desired, &base)
	}))
	want := sha256.Sum256([]byte(`{"test": true}`))
	if string(desired) != string(want[:]) || len(base) != 32 {
		t.Fatalf("desired %x (want %x), base %x", desired, want, base)
	}

	// A plan with no steps, or one not sealed, does not commit.
	_, err = plan(f, "rita", "empty", nil, nil)
	wantState(t, err, sqlCheck)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return planIn(ctx, tx, uuid.New(), "unsealed", twoStage(), nil)
	})
	wantState(t, err, sqlCheck)
}

func TestAPlanIsImmutableOnceSealed(t *testing.T) {
	f := registrytest.New(t)
	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	for _, sql := range []string{
		`UPDATE eacp.change_sets SET desired = '{"other": 1}' WHERE id = $1`,
		`UPDATE eacp.change_sets SET base_digest = sha256('x') WHERE id = $1`,
		`INSERT INTO eacp.change_set_steps (tenant_id, change_set_id, ordinal, address, op, stage, payload)
		 VALUES (eacp.current_tenant_id(), $1, 9, 'agent.late', 'create', 'submit', '{}')`,
		`INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
		 VALUES (eacp.current_tenant_id(), $1, 'agent', gen_random_uuid())`,
		`SELECT eacp.change_set_seal($1)`,
	} {
		wantState(t, f.Exec("erin", sql, id), sqlBadState)
	}
}

func TestOneOpenChangeSetPerBundle(t *testing.T) {
	f := registrytest.New(t)
	first, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	_, err = plan(f, "erin", "ledger", twoStage(), nil)
	wantState(t, err, sqlUnique)
	wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'SUPERSEDED' WHERE id = $1`, first), sqlCheck)
	ok(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'SUPERSEDED', close_reason = 'replanned' WHERE id = $1`, first))
	_, err = plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	if n := auditCount(t, f, "change_set.superseded"); n != 1 {
		t.Fatalf("superseded events = %d", n)
	}
}

func TestSubmitChecksTheDigestAndRunsStepsOnceInOrder(t *testing.T) {
	f := registrytest.New(t)
	tool := f.ActiveTool(t, "erp", "po")
	id, err := plan(f, "erin", "ledger", twoStage(), []rawRef{{"connector", tool.Connector}})
	ok(t, err)

	// A registry change to a ref makes the plan stale.
	ok(t, f.Exec("erin", `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, 'extra')`, tool.Connector))
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "SUBMITTED") })
	wantState(t, err, sqlStale)

	ok(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'REJECTED', close_reason = 'stale' WHERE id = $1`, id))
	id, err = plan(f, "erin", "ledger", twoStage(), []rawRef{{"connector", tool.Connector}})
	ok(t, err)

	// Steps run in order, once, only in the submitting transaction.
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 2)
	})
	wantState(t, err, sqlBadState)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1) // step 2 never runs: the commit check refuses
	})
	wantState(t, err, sqlCheck)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1, 2); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1)
	})
	wantState(t, err, sqlBadState)
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1, 2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	}))
	// After commit, nobody runs a step of it again, and approve steps wait for approval.
	wantState(t, f.Exec("erin", `UPDATE eacp.change_set_steps SET object_id = gen_random_uuid()
		WHERE change_set_id = $1 AND ordinal = 3`, id), sqlBadState)
	if n := auditCount(t, f, "change_set.submitted"); n != 1 {
		t.Fatalf("submitted events = %d", n)
	}
}

func TestApprovalIsASecondPersonAgainstTheSealedDigest(t *testing.T) {
	f := registrytest.New(t)
	agent := f.NewAgent(t, "buyer")
	submit := func(actor string) uuid.UUID {
		id, err := plan(f, actor, "b-"+actor, twoStage(), []rawRef{{"version", agent.Version}})
		ok(t, err)
		ok(t, inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
				return err
			}
			if err := runSteps(ctx, tx, id, 1, 2); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
			return err
		}))
		return id
	}
	approve := func(actor string, id uuid.UUID) error {
		return inTx(f, actor, func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "APPLIED"); err != nil {
				return err
			}
			return runSteps(ctx, tx, id, 3)
		})
	}

	id := submit("rita")
	wantState(t, approve("rita", id), sqlForbidden) // the submitter
	wantState(t, approve("erin", id), sqlForbidden) // no approver role
	ok(t, approve("ravi", id))
	if n := auditCount(t, f, "change_set.applied"); n != 1 {
		t.Fatalf("applied events = %d", n)
	}

	// The digest sealed at submission covers the refs: a change makes it stale.
	id = submit("erin")
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'incident' WHERE id = $1`,
		agent.Version))
	wantState(t, approve("rita", id), sqlStale)
}

func TestDirectApplyOnlyWithoutApproveSteps(t *testing.T) {
	f := registrytest.New(t)
	single := []rawStep{{"connector.ledger", "create", "submit"}}
	id, err := plan(f, "erin", "one", single, nil)
	ok(t, err)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "SUBMITTED") })
	wantState(t, err, sqlBadState)
	ok(t, inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "APPLIED"); err != nil {
			return err
		}
		return runSteps(ctx, tx, id, 1)
	}))

	id, err = plan(f, "erin", "two", twoStage(), nil)
	ok(t, err)
	err = inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error { return setState(ctx, tx, id, "APPLIED") })
	wantState(t, err, sqlBadState)
}

func TestClosedChangeSetsAreTerminalAndRejectionNeedsAReason(t *testing.T) {
	f := registrytest.New(t)
	id, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = 'REJECTED' WHERE id = $1`, id), sqlCheck)
	ok(t, f.Exec("rita", `UPDATE eacp.change_sets SET state = 'REJECTED', close_reason = 'not now' WHERE id = $1`, id))
	for _, to := range []string{"PLANNED", "SUBMITTED", "APPLIED", "SUPERSEDED"} {
		wantState(t, f.Exec("erin", `UPDATE eacp.change_sets SET state = $2 WHERE id = $1`, id, to), sqlBadState)
	}
	if n := auditCount(t, f, "change_set.rejected"); n != 1 {
		t.Fatalf("rejected events = %d", n)
	}
}

func TestBundleResourcesAreWrittenOnlyByTheExecutingChangeSet(t *testing.T) {
	f := registrytest.New(t)
	conn := f.ActiveTool(t, "erp", "po").Connector
	steps := []rawStep{{"connector.erp", "import", "submit"}}
	id, err := plan(f, "erin", "erp", steps, []rawRef{{"connector", conn}})
	ok(t, err)
	manage := `INSERT INTO eacp.bundle_resources (tenant_id, bundle_id, address, kind, object_id, change_set_id)
		SELECT eacp.current_tenant_id(), bundle_id, 'connector.erp', 'connector', $2, id
		FROM eacp.change_sets WHERE id = $1`
	wantState(t, f.Exec("erin", manage, id, conn), sqlBadState) // not executing

	// importAndManage applies change set cs (its one import step produces
	// conn) and tries to manage object as connector.erp, in one transaction.
	importAndManage := func(cs, object uuid.UUID) error {
		return inTx(f, "erin", func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, cs, "APPLIED"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eacp.change_set_steps SET object_id = $2
				WHERE change_set_id = $1 AND ordinal = 1`, cs, conn); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, manage, cs, object)
			return err
		})
	}
	wantState(t, importAndManage(id, uuid.New()), sqlBadState) // no step produced that object
	ok(t, importAndManage(id, conn))

	// An object belongs to at most one bundle.
	other, err := plan(f, "erin", "erp-other", steps, []rawRef{{"connector", conn}})
	ok(t, err)
	wantState(t, importAndManage(other, conn), sqlUnique)
}

func TestChangeSetsAreTenantIsolated(t *testing.T) {
	f := registrytest.New(t)
	other := f.ForTenant(t, pgtest.TenantB)
	_, err := plan(f, "erin", "ledger", twoStage(), nil)
	ok(t, err)
	var n int
	ok(t, inTx(other, "erin", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM eacp.change_sets) + (SELECT count(*) FROM eacp.bundles)
			+ (SELECT count(*) FROM eacp.change_set_steps)`).Scan(&n)
	}))
	if n != 0 {
		t.Fatalf("tenant B sees %d change-set rows", n)
	}
	_, err = plan(other, "erin", "ledger", twoStage(), nil) // same bundle name, other tenant
	ok(t, err)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Test'`
Expected: FAIL. Errors mention `relation "eacp.bundles" does not exist`.

- [ ] **Step 4: Write the migration**

`migrations/00020_governance_as_code.sql`:

```sql
-- Phase 20 (ADR-026): Governance-as-Code. A bundle's desired state is
-- planned into a change set. Submit and approve run its steps as the same
-- registry writes the API makes, so every registry trigger still decides
-- each one. PostgreSQL computes every digest; a change set whose digest no
-- longer matches the registry is stale and runs nothing. Nothing is deleted.
-- +goose Up

CREATE TABLE eacp.bundles (
    tenant_id  uuid        NOT NULL REFERENCES eacp.tenants (id),
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    created_by uuid        NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, id),
    UNIQUE (tenant_id, name),
    FOREIGN KEY (tenant_id, created_by) REFERENCES eacp.principals (tenant_id, id)
);

CREATE TABLE eacp.change_sets (
    tenant_id      uuid        NOT NULL,
    id             uuid        NOT NULL,
    bundle_id      uuid        NOT NULL,
    state          text        NOT NULL CHECK (state IN ('PLANNED', 'SUBMITTED', 'APPLIED', 'REJECTED', 'SUPERSEDED')),
    desired        jsonb       NOT NULL CHECK (jsonb_typeof(desired) = 'object' AND octet_length(desired::text) <= 1048576),
    desired_digest bytea       NOT NULL CHECK (octet_length(desired_digest) = 32),
    prune          boolean     NOT NULL,
    base_digest    bytea       CHECK (octet_length(base_digest) = 32),
    sealed_digest  bytea       CHECK (octet_length(sealed_digest) = 32),
    planned_by     uuid        NOT NULL,
    planned_at     timestamptz NOT NULL,
    planned_xact   xid8        NOT NULL,
    submitted_by   uuid,
    submitted_at   timestamptz,
    submitted_xact xid8,
    approved_by    uuid,
    approved_at    timestamptz,
    approved_xact  xid8,
    closed_by      uuid,
    closed_at      timestamptz,
    close_reason   text        CHECK (length(close_reason) <= 1024),
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, bundle_id) REFERENCES eacp.bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, planned_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, submitted_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, approved_by) REFERENCES eacp.principals (tenant_id, id),
    FOREIGN KEY (tenant_id, closed_by) REFERENCES eacp.principals (tenant_id, id)
);
-- One open change set per bundle: the lock Terraform keeps in a state file.
CREATE UNIQUE INDEX change_sets_one_open ON eacp.change_sets (tenant_id, bundle_id)
    WHERE state IN ('PLANNED', 'SUBMITTED');
CREATE INDEX change_sets_by_bundle ON eacp.change_sets (tenant_id, bundle_id, planned_at DESC);

CREATE TABLE eacp.change_set_steps (
    tenant_id     uuid    NOT NULL,
    change_set_id uuid    NOT NULL,
    ordinal       integer NOT NULL CHECK (ordinal BETWEEN 1 AND 2000),
    address       text    NOT NULL CHECK (address ~
        '^(connector|tool|contract|agent|version|allowlist)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$'),
    op            text    NOT NULL CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import')),
    stage         text    NOT NULL CHECK (stage IN ('submit', 'approve')),
    payload       jsonb   NOT NULL CHECK (jsonb_typeof(payload) = 'object' AND octet_length(payload::text) <= 65536),
    object_id     uuid,
    executed_xact xid8,
    PRIMARY KEY (tenant_id, change_set_id, ordinal),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id),
    CHECK ((object_id IS NULL) = (executed_xact IS NULL))
);

CREATE TABLE eacp.change_set_refs (
    tenant_id     uuid NOT NULL,
    change_set_id uuid NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('connector', 'tool', 'contract', 'agent', 'version', 'allowlist',
                                                'principal', 'group')),
    object_id     uuid NOT NULL,
    PRIMARY KEY (tenant_id, change_set_id, kind, object_id),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id)
);

-- The state: which registry object an address of a bundle manages.
CREATE TABLE eacp.bundle_resources (
    tenant_id     uuid        NOT NULL,
    bundle_id     uuid        NOT NULL,
    address       text        NOT NULL,
    kind          text        NOT NULL CHECK (kind IN ('connector', 'tool', 'agent', 'version')),
    object_id     uuid        NOT NULL,
    change_set_id uuid        NOT NULL,
    managed_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, bundle_id, address),
    UNIQUE (tenant_id, kind, object_id),
    FOREIGN KEY (tenant_id, bundle_id) REFERENCES eacp.bundles (tenant_id, id),
    FOREIGN KEY (tenant_id, change_set_id) REFERENCES eacp.change_sets (tenant_id, id)
);

-- +goose StatementBegin
CREATE FUNCTION eacp.assert_bundle_role(a uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF a IS NULL THEN
        RAISE EXCEPTION 'a change set needs a principal' USING ERRCODE = '42501';
    END IF;
    PERFORM eacp.assert_role(a, 'registry_editor', 'registry_approver', 'admin');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.bundles_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
BEGIN
    PERFORM eacp.assert_bundle_role(a);
    NEW.created_by := a;
    NEW.created_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The part of a registry object a plan depends on, as text, or 'absent'.
-- Under RLS it sees only the current tenant.
CREATE FUNCTION eacp.change_set_ref_row(p_kind text, p_id uuid) RETURNS text
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE
    r text;
BEGIN
    CASE p_kind
    WHEN 'connector' THEN
        SELECT jsonb_build_array(c.id, c.name, c.protocol, c.endpoint, c.secret_ref,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(t.id, t.name) ORDER BY t.id), '[]'::jsonb)
                  FROM eacp.tools t WHERE t.tenant_id = c.tenant_id AND t.connector_id = c.id))::text
          INTO r FROM eacp.connectors c WHERE c.id = p_id;
    WHEN 'tool' THEN
        SELECT jsonb_build_array(t.id, t.connector_id, t.name, t.origin, t.active_contract_id, t.definition_id,
               t.quarantined_at IS NULL)::text
          INTO r FROM eacp.tools t WHERE t.id = p_id;
    WHEN 'contract' THEN
        SELECT jsonb_build_array(ct.id, ct.tool_id, ct.revoked_at IS NULL)::text
          INTO r FROM eacp.tool_contracts ct WHERE ct.id = p_id;
    WHEN 'agent' THEN
        SELECT jsonb_build_array(a.id, a.name, a.display_name, a.environment, a.risk_class,
               a.owner_principal_id, a.owner_group_id,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(v.id, v.state, v.active_allowlist_id) ORDER BY v.version),
                                '[]'::jsonb)
                  FROM eacp.agent_versions v WHERE v.tenant_id = a.tenant_id AND v.agent_id = a.id))::text
          INTO r FROM eacp.agents a WHERE a.id = p_id;
    WHEN 'version' THEN
        SELECT jsonb_build_array(v.id, v.agent_id, v.version, v.runtime, v.code_ref, v.state,
               v.active_allowlist_id)::text
          INTO r FROM eacp.agent_versions v WHERE v.id = p_id;
    WHEN 'allowlist' THEN
        SELECT jsonb_build_array(al.id, al.agent_version_id, al.tool_ids)::text
          INTO r FROM eacp.agent_allowlists al WHERE al.id = p_id;
    WHEN 'principal' THEN
        SELECT jsonb_build_array(p.id, p.name, p.disabled_at IS NULL)::text
          INTO r FROM eacp.principals p WHERE p.id = p_id;
    WHEN 'group' THEN
        SELECT jsonb_build_array(g.id, g.name)::text
          INTO r FROM eacp.groups g WHERE g.id = p_id;
    ELSE
        RAISE EXCEPTION 'unknown ref kind %', p_kind USING ERRCODE = '23514';
    END CASE;
    RETURN COALESCE(r, 'absent');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The digest of what a change set depends on: its refs and the objects its
-- bundle manages.
CREATE FUNCTION eacp.change_set_digest(p_change_set uuid) RETURNS bytea
    LANGUAGE sql STABLE
    AS $$
    SELECT sha256(convert_to(COALESCE(string_agg(x.line, E'\n' ORDER BY x.line), ''), 'UTF8'))
      FROM (SELECT r.kind || ' ' || r.object_id || ' ' || eacp.change_set_ref_row(r.kind, r.object_id) AS line
              FROM eacp.change_set_refs r WHERE r.change_set_id = p_change_set
            UNION ALL
            SELECT 'managed ' || br.address || ' ' || br.object_id
              FROM eacp.bundle_resources br
              JOIN eacp.change_sets cs ON cs.tenant_id = br.tenant_id AND cs.bundle_id = br.bundle_id
             WHERE cs.id = p_change_set) x
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.change_sets_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    has_approve boolean;
BEGIN
    PERFORM eacp.assert_bundle_role(a);
    IF TG_OP = 'INSERT' THEN
        IF NEW.state <> 'PLANNED' THEN
            RAISE EXCEPTION 'a change set starts PLANNED' USING ERRCODE = '23514';
        END IF;
        NEW.desired_digest := sha256(convert_to(NEW.desired::text, 'UTF8'));
        NEW.base_digest := NULL;
        NEW.sealed_digest := NULL;
        NEW.planned_by := a;
        NEW.planned_at := now();
        NEW.planned_xact := pg_current_xact_id();
        NEW.submitted_by := NULL; NEW.submitted_at := NULL; NEW.submitted_xact := NULL;
        NEW.approved_by := NULL; NEW.approved_at := NULL; NEW.approved_xact := NULL;
        NEW.closed_by := NULL; NEW.closed_at := NULL; NEW.close_reason := NULL;
        RETURN NEW;
    END IF;

    IF (NEW.tenant_id, NEW.id, NEW.bundle_id, NEW.desired, NEW.desired_digest, NEW.prune,
        NEW.planned_by, NEW.planned_at, NEW.planned_xact)
       IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.bundle_id, OLD.desired, OLD.desired_digest, OLD.prune,
        OLD.planned_by, OLD.planned_at, OLD.planned_xact) THEN
        RAISE EXCEPTION 'a change set''s plan is immutable' USING ERRCODE = '55000';
    END IF;

    IF NEW.state = OLD.state THEN
        -- A seal: a digest written once, equal to PostgreSQL's own.
        IF (NEW.submitted_by, NEW.submitted_at, NEW.submitted_xact, NEW.approved_by, NEW.approved_at,
            NEW.approved_xact, NEW.closed_by, NEW.closed_at, NEW.close_reason)
           IS DISTINCT FROM (OLD.submitted_by, OLD.submitted_at, OLD.submitted_xact, OLD.approved_by,
            OLD.approved_at, OLD.approved_xact, OLD.closed_by, OLD.closed_at, OLD.close_reason) THEN
            RAISE EXCEPTION 'only a state change records who moved a change set' USING ERRCODE = '55000';
        END IF;
        IF NEW.base_digest IS DISTINCT FROM OLD.base_digest AND (
               OLD.base_digest IS NOT NULL OR OLD.state <> 'PLANNED'
               OR OLD.planned_xact <> pg_current_xact_id() OR OLD.planned_by <> a
               OR NEW.base_digest IS DISTINCT FROM eacp.change_set_digest(OLD.id)) THEN
            RAISE EXCEPTION 'a plan is sealed once, by the transaction that planned it' USING ERRCODE = '55000';
        END IF;
        IF NEW.sealed_digest IS DISTINCT FROM OLD.sealed_digest THEN
            IF OLD.sealed_digest IS NOT NULL OR OLD.state <> 'SUBMITTED'
               OR OLD.submitted_xact <> pg_current_xact_id() OR OLD.submitted_by <> a
               OR NEW.sealed_digest IS DISTINCT FROM eacp.change_set_digest(OLD.id) THEN
                RAISE EXCEPTION 'a submission is sealed once, by the transaction that submitted it'
                    USING ERRCODE = '55000';
            END IF;
            IF EXISTS (SELECT 1 FROM eacp.change_set_steps s
                        WHERE s.tenant_id = OLD.tenant_id AND s.change_set_id = OLD.id AND s.stage = 'submit'
                          AND (s.object_id IS NULL OR NOT EXISTS (
                               SELECT 1 FROM eacp.change_set_refs r
                                WHERE r.tenant_id = s.tenant_id AND r.change_set_id = s.change_set_id
                                  AND r.kind = split_part(s.address, '.', 1) AND r.object_id = s.object_id))) THEN
                RAISE EXCEPTION 'every submit step runs, and its object joins the refs, before the seal'
                    USING ERRCODE = '55000';
            END IF;
        END IF;
        RETURN NEW;
    END IF;

    -- A state change. Only the guard writes the who/when/xact columns.
    NEW.base_digest := OLD.base_digest;
    NEW.sealed_digest := OLD.sealed_digest;
    NEW.submitted_by := OLD.submitted_by; NEW.submitted_at := OLD.submitted_at; NEW.submitted_xact := OLD.submitted_xact;
    NEW.approved_by := OLD.approved_by; NEW.approved_at := OLD.approved_at; NEW.approved_xact := OLD.approved_xact;
    NEW.closed_by := OLD.closed_by; NEW.closed_at := OLD.closed_at;
    SELECT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = OLD.tenant_id AND change_set_id = OLD.id AND stage = 'approve')
      INTO has_approve;

    IF OLD.state = 'PLANNED' AND NEW.state IN ('SUBMITTED', 'APPLIED') THEN
        IF OLD.base_digest IS NULL THEN
            RAISE EXCEPTION 'an unsealed plan cannot be submitted' USING ERRCODE = '55000';
        END IF;
        IF has_approve AND NEW.state = 'APPLIED' THEN
            RAISE EXCEPTION 'change set % has approve-stage steps: a second person applies it', OLD.id
                USING ERRCODE = '55000';
        ELSIF NOT has_approve AND NEW.state = 'SUBMITTED' THEN
            RAISE EXCEPTION 'change set % has no approve-stage steps: submitting applies it', OLD.id
                USING ERRCODE = '55000';
        END IF;
        IF eacp.change_set_digest(OLD.id) <> OLD.base_digest THEN
            RAISE EXCEPTION 'change set % is stale: the registry changed since it was planned', OLD.id
                USING ERRCODE = '40001';
        END IF;
        NEW.submitted_by := a;
        NEW.submitted_at := now();
        NEW.submitted_xact := pg_current_xact_id();
        NEW.close_reason := NULL;
    ELSIF OLD.state = 'SUBMITTED' AND NEW.state = 'APPLIED' THEN
        PERFORM eacp.assert_role(a, 'registry_approver', 'admin');
        PERFORM eacp.assert_distinct(a, OLD.submitted_by, 'the submitter of the change set');
        IF OLD.sealed_digest IS NULL THEN
            RAISE EXCEPTION 'an unsealed submission cannot be approved' USING ERRCODE = '55000';
        END IF;
        IF eacp.change_set_digest(OLD.id) <> OLD.sealed_digest THEN
            RAISE EXCEPTION 'change set % is stale: the registry changed since it was submitted', OLD.id
                USING ERRCODE = '40001';
        END IF;
        NEW.approved_by := a;
        NEW.approved_at := now();
        NEW.approved_xact := pg_current_xact_id();
        NEW.close_reason := NULL;
    ELSIF OLD.state IN ('PLANNED', 'SUBMITTED') AND NEW.state = 'REJECTED' THEN
        PERFORM eacp.require_reason(NEW.close_reason, 'rejecting a change set');
        NEW.closed_by := a;
        NEW.closed_at := now();
    ELSIF OLD.state = 'PLANNED' AND NEW.state = 'SUPERSEDED' THEN
        PERFORM eacp.require_reason(NEW.close_reason, 'superseding a change set');
        NEW.closed_by := a;
        NEW.closed_at := now();
    ELSE
        RAISE EXCEPTION 'a change set cannot move from % to %', OLD.state, NEW.state USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Steps are written by the planning transaction before its seal, and each
-- is run once, in order, by the transaction that submits or approves its
-- stage.
CREATE FUNCTION eacp.change_set_steps_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
    ok boolean;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF a IS NULL OR cs.state <> 'PLANNED' OR cs.planned_by <> a OR cs.planned_xact <> pg_current_xact_id()
           OR cs.base_digest IS NOT NULL THEN
            RAISE EXCEPTION 'steps are added only by the transaction that plans the change set, before its seal'
                USING ERRCODE = '55000';
        END IF;
        NEW.object_id := NULL;
        NEW.executed_xact := NULL;
        RETURN NEW;
    END IF;
    IF (NEW.tenant_id, NEW.change_set_id, NEW.ordinal, NEW.address, NEW.op, NEW.stage, NEW.payload)
       IS DISTINCT FROM (OLD.tenant_id, OLD.change_set_id, OLD.ordinal, OLD.address, OLD.op, OLD.stage, OLD.payload) THEN
        RAISE EXCEPTION 'a planned step is immutable' USING ERRCODE = '55000';
    END IF;
    IF OLD.object_id IS NOT NULL OR NEW.object_id IS NULL THEN
        RAISE EXCEPTION 'a step runs once' USING ERRCODE = '55000';
    END IF;
    ok := CASE OLD.stage
        WHEN 'submit' THEN cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a
                           AND cs.submitted_xact = pg_current_xact_id() AND cs.sealed_digest IS NULL
                           AND cs.approved_xact IS NULL
        WHEN 'approve' THEN cs.state = 'APPLIED' AND cs.approved_by = a
                            AND cs.approved_xact = pg_current_xact_id()
    END;
    IF NOT COALESCE(ok, false) THEN
        RAISE EXCEPTION 'a step runs only in the transaction that submits or approves its stage'
            USING ERRCODE = '55000';
    END IF;
    IF EXISTS (SELECT 1 FROM eacp.change_set_steps
                WHERE tenant_id = OLD.tenant_id AND change_set_id = OLD.change_set_id AND stage = OLD.stage
                  AND ordinal < OLD.ordinal AND object_id IS NULL) THEN
        RAISE EXCEPTION 'steps run in order' USING ERRCODE = '55000';
    END IF;
    NEW.executed_xact := pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION eacp.change_set_refs_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF a IS NULL OR NOT (
          (cs.state = 'PLANNED' AND cs.planned_by = a AND cs.planned_xact = pg_current_xact_id()
           AND cs.base_digest IS NULL)
       OR (cs.state = 'SUBMITTED' AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.sealed_digest IS NULL)) THEN
        RAISE EXCEPTION 'refs are recorded only before the plan or its submission is sealed'
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Seals a change set: the planning transaction records base_digest; the
-- submitting transaction adds the objects its steps produced to the refs and
-- records sealed_digest, which the approval must match.
CREATE FUNCTION eacp.change_set_seal(p_change_set uuid) RETURNS bytea
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    d bytea;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE id = p_change_set FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'no such change set' USING ERRCODE = '23503';
    END IF;
    IF NOT ((cs.state = 'PLANNED' AND cs.base_digest IS NULL)
            OR (cs.state = 'SUBMITTED' AND cs.sealed_digest IS NULL)) THEN
        RAISE EXCEPTION 'change set % is % and already sealed', cs.id, cs.state USING ERRCODE = '55000';
    END IF;
    IF cs.state = 'SUBMITTED' THEN
        INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
        SELECT tenant_id, change_set_id, split_part(address, '.', 1), object_id
          FROM eacp.change_set_steps
         WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id AND stage = 'submit' AND object_id IS NOT NULL
        ON CONFLICT DO NOTHING;
        d := eacp.change_set_digest(cs.id);
        UPDATE eacp.change_sets SET sealed_digest = d WHERE tenant_id = cs.tenant_id AND id = cs.id;
    ELSE
        d := eacp.change_set_digest(cs.id);
        UPDATE eacp.change_sets SET base_digest = d WHERE tenant_id = cs.tenant_id AND id = cs.id;
    END IF;
    RETURN d;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An address manages an object only when the executing change set of its
-- bundle created or imported that object in this transaction.
CREATE FUNCTION eacp.bundle_resources_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a uuid := eacp.current_actor_id();
    cs eacp.change_sets%ROWTYPE;
BEGIN
    IF TG_OP = 'UPDATE' AND (NEW.tenant_id, NEW.bundle_id, NEW.address)
                            IS DISTINCT FROM (OLD.tenant_id, OLD.bundle_id, OLD.address) THEN
        RAISE EXCEPTION 'a managed address is permanent' USING ERRCODE = '55000';
    END IF;
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.change_set_id;
    IF NOT FOUND OR cs.bundle_id <> NEW.bundle_id OR a IS NULL OR NOT (
          (cs.state IN ('SUBMITTED', 'APPLIED') AND cs.submitted_by = a AND cs.submitted_xact = pg_current_xact_id()
           AND cs.approved_xact IS NULL AND cs.sealed_digest IS NULL)
       OR (cs.state = 'APPLIED' AND cs.approved_by = a AND cs.approved_xact = pg_current_xact_id())) THEN
        RAISE EXCEPTION 'only the executing change set of a bundle manages its objects' USING ERRCODE = '55000';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps
                    WHERE tenant_id = NEW.tenant_id AND change_set_id = cs.id AND address = NEW.address
                      AND op IN ('create', 'import') AND object_id = NEW.object_id
                      AND executed_xact = pg_current_xact_id()) THEN
        RAISE EXCEPTION 'a managed object is one this change set created or imported' USING ERRCODE = '55000';
    END IF;
    NEW.kind := split_part(NEW.address, '.', 1);
    IF NEW.kind NOT IN ('connector', 'tool', 'agent', 'version') THEN
        RAISE EXCEPTION 'a bundle manages connectors, tools, agents and versions' USING ERRCODE = '23514';
    END IF;
    IF eacp.change_set_ref_row(NEW.kind, NEW.object_id) = 'absent' THEN
        RAISE EXCEPTION 'no such % in this tenant', NEW.kind USING ERRCODE = '23503';
    END IF;
    NEW.managed_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- At commit: a plan has steps and is sealed; an executed stage ran every
-- step; a submission is sealed. The journal entry comes last.
CREATE FUNCTION eacp.change_sets_commit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    cs eacp.change_sets%ROWTYPE;
    bundle_name text;
    run_stage text;
    what text;
    who uuid;
    step_list jsonb;
BEGIN
    SELECT * INTO cs FROM eacp.change_sets WHERE tenant_id = NEW.tenant_id AND id = NEW.id;
    SELECT name INTO bundle_name FROM eacp.bundles WHERE tenant_id = cs.tenant_id AND id = cs.bundle_id;
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM eacp.change_set_steps WHERE tenant_id = cs.tenant_id AND change_set_id = cs.id) THEN
            RAISE EXCEPTION 'a change set has at least one step' USING ERRCODE = '23514';
        END IF;
        IF cs.base_digest IS NULL THEN
            RAISE EXCEPTION 'a plan is sealed before it commits' USING ERRCODE = '23514';
        END IF;
        what := 'change_set.planned';
        who := cs.planned_by;
    ELSE
        what := 'change_set.' || lower(NEW.state);
        CASE NEW.state
        WHEN 'SUBMITTED' THEN
            who := cs.submitted_by;
            run_stage := 'submit';
            IF cs.sealed_digest IS NULL THEN
                RAISE EXCEPTION 'a submission is sealed before it commits' USING ERRCODE = '23514';
            END IF;
        WHEN 'APPLIED' THEN
            IF cs.approved_xact IS NOT NULL THEN
                who := cs.approved_by;
                run_stage := 'approve';
            ELSE
                who := cs.submitted_by;
                run_stage := 'submit';
            END IF;
        ELSE
            who := cs.closed_by;
        END CASE;
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.object_id IS NULL) THEN
            RAISE EXCEPTION 'every % step of a change set runs before it commits', run_stage USING ERRCODE = '23514';
        END IF;
    END IF;
    SELECT jsonb_agg(jsonb_build_object('ordinal', s.ordinal, 'address', s.address, 'op', s.op, 'stage', s.stage,
                                        'object_id', s.object_id) ORDER BY s.ordinal)
      INTO step_list FROM eacp.change_set_steps s WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id;
    INSERT INTO eacp.audit_events (tenant_id, payload)
    VALUES (cs.tenant_id, convert_to(jsonb_build_object(
        'v', 1, 'actor', jsonb_build_object('kind', 'principal', 'id', who),
        'action', what,
        'subject', jsonb_build_object('type', 'change_set', 'id', cs.id),
        'reason', COALESCE(cs.close_reason, ''),
        'data', jsonb_build_object('bundle', bundle_name, 'desired_digest', encode(cs.desired_digest, 'hex'),
                                   'base_digest', encode(cs.base_digest, 'hex'),
                                   'sealed_digest', encode(cs.sealed_digest, 'hex'),
                                   'steps', COALESCE(step_list, '[]'::jsonb)))::text, 'UTF8'));
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER bundles_guard BEFORE INSERT ON eacp.bundles
    FOR EACH ROW EXECUTE FUNCTION eacp.bundles_guard();
CREATE TRIGGER change_sets_guard BEFORE INSERT OR UPDATE ON eacp.change_sets
    FOR EACH ROW EXECUTE FUNCTION eacp.change_sets_guard();
CREATE CONSTRAINT TRIGGER change_sets_commit_insert AFTER INSERT ON eacp.change_sets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION eacp.change_sets_commit();
CREATE CONSTRAINT TRIGGER change_sets_commit_update AFTER UPDATE ON eacp.change_sets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state)
    EXECUTE FUNCTION eacp.change_sets_commit();
CREATE TRIGGER change_set_steps_guard BEFORE INSERT OR UPDATE ON eacp.change_set_steps
    FOR EACH ROW EXECUTE FUNCTION eacp.change_set_steps_guard();
CREATE TRIGGER change_set_refs_guard BEFORE INSERT ON eacp.change_set_refs
    FOR EACH ROW EXECUTE FUNCTION eacp.change_set_refs_guard();
CREATE TRIGGER bundle_resources_guard BEFORE INSERT OR UPDATE ON eacp.bundle_resources
    FOR EACH ROW EXECUTE FUNCTION eacp.bundle_resources_guard();

ALTER TABLE eacp.bundles ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.bundles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.bundles USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_sets USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_set_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_set_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_set_steps USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.change_set_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.change_set_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.change_set_refs USING (tenant_id = eacp.current_tenant_id());
ALTER TABLE eacp.bundle_resources ENABLE ROW LEVEL SECURITY;
ALTER TABLE eacp.bundle_resources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON eacp.bundle_resources USING (tenant_id = eacp.current_tenant_id());
REVOKE UPDATE ON eacp.bundles FROM eacp_app;
REVOKE UPDATE ON eacp.change_set_refs FROM eacp_app;

-- +goose Down
DROP TABLE eacp.bundle_resources;
DROP TABLE eacp.change_set_refs;
DROP TABLE eacp.change_set_steps;
DROP TABLE eacp.change_sets;
DROP TABLE eacp.bundles;
DROP FUNCTION eacp.change_sets_commit();
DROP FUNCTION eacp.bundle_resources_guard();
DROP FUNCTION eacp.change_set_seal(uuid);
DROP FUNCTION eacp.change_set_refs_guard();
DROP FUNCTION eacp.change_set_steps_guard();
DROP FUNCTION eacp.change_sets_guard();
DROP FUNCTION eacp.change_set_digest(uuid);
DROP FUNCTION eacp.change_set_ref_row(text, uuid);
DROP FUNCTION eacp.bundles_guard();
DROP FUNCTION eacp.assert_bundle_role(uuid);
```

- [ ] **Step 5: Review the new tables in the RLS catalog**

In `internal/storage/rls_catalog_test.go`, after the `releases` block (the one ending `t.Errorf("reviewed release tables missing: %v", got)`), add:

```go
	bundles := []string{"bundle_resources", "bundles", "change_set_refs", "change_set_steps", "change_sets"}
	if got := strs(`SELECT relname FROM pg_class WHERE relnamespace = 'eacp'::regnamespace
		AND relkind = 'r' AND relname IN ('bundle_resources', 'bundles', 'change_set_refs', 'change_set_steps', 'change_sets')
		ORDER BY relname`); !slices.Equal(got, bundles) {
		t.Errorf("reviewed bundle tables missing: %v", got)
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/bundle/ ./internal/storage/`
Expected: PASS, including `TestEveryDownMigrationRestoresThePreviousSchema` and the RLS catalog test.

- [ ] **Step 7: Commit**

```bash
git add migrations/00020_governance_as_code.sql internal/bundle/doc.go internal/bundle/schema_test.go internal/storage/rls_catalog_test.go
git commit -m "feat(bundle): change-set schema, guards, digests and journal (ADR-026)"
```

---

### Task 2: `storage.InTenantSnapshotTx` and `registry.Tx`

**Files:**
- Modify: `internal/storage/storage.go` (after `InTenantReadTx`, around line 118)
- Create: `internal/registry/tx.go`
- Modify: `internal/registry/agents.go` (`RegisterAgent`, `RegisterVersion`, `ProposeAllowlist`, `ActivateAllowlist`, `TransitionVersion`)
- Modify: `internal/registry/connectors.go` (`RegisterConnector`, `RegisterTool`, `ProposeContract`, `ActivateContract`, `RevokeContract`)
- Test: `internal/storage/storage_test.go` (add one test), plus the existing registry tests

**Interfaces:**
- Produces: `storage.InTenantSnapshotTx(ctx, pool, tenantID string, fn func(pgx.Tx) error) error`
- Produces: `registry.Tx{Tx pgx.Tx}` with methods `RegisterConnector(ctx, NewConnector) (Connector, error)`, `RegisterTool(ctx, connectorID uuid.UUID, name string) (uuid.UUID, error)`, `ProposeContract(ctx, toolID uuid.UUID, c Contract) (uuid.UUID, error)`, `ActivateContract(ctx, toolID, contractID uuid.UUID) error`, `RevokeContract(ctx, contractID uuid.UUID, reason string) error`, `RegisterAgent(ctx, NewAgent) (Agent, error)`, `RegisterVersion(ctx, agentID uuid.UUID, NewVersion) (Version, error)`, `ProposeAllowlist(ctx, versionID uuid.UUID, tools []string) (uuid.UUID, error)`, `ActivateAllowlist(ctx, versionID, allowlistID uuid.UUID) error`, `TransitionVersion(ctx, versionID uuid.UUID, to State, reason string) error`
- Produces: `registry.MapErr(err error) error`

- [ ] **Step 1: Write the failing snapshot test**

Append to `internal/storage/storage_test.go` (it already imports `pgtest`, `pgx`, `context`; add imports if missing):

```go
// InTenantSnapshotTx reads one snapshot and may write: a row committed by
// another transaction after its first statement stays invisible.
func TestInTenantSnapshotTxReadsOneSnapshotAndWrites(t *testing.T) {
	db := pgtest.Migrated(t)
	ctx := context.Background()
	app := pgtest.Pool(t, db.AppDSN)
	owner := pgtest.Pool(t, db.OwnerDSN)
	err := storage.InTenantSnapshotTx(ctx, app, pgtest.TenantA, func(tx pgx.Tx) error {
		var before int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.principals`).Scan(&before); err != nil {
			return err
		}
		if err := storage.InTenantTx(ctx, owner, pgtest.TenantA, func(o pgx.Tx) error {
			_, err := o.Exec(ctx, `INSERT INTO eacp.principals (tenant_id, kind, name, display_name)
				VALUES (eacp.current_tenant_id(), 'service', 'late-svc', 'late')`)
			return err
		}); err != nil {
			return err
		}
		var after int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM eacp.principals`).Scan(&after); err != nil {
			return err
		}
		if after != before {
			t.Errorf("snapshot saw %d then %d principals", before, after)
		}
		var readOnly string
		if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		if readOnly != "off" {
			t.Errorf("transaction_read_only = %s", readOnly)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

Before writing it, check how `storage_test.go` names its package and imports (`storage_test` with `eacp/internal/storage`); match it.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -race -count=1 ./internal/storage/ -run TestInTenantSnapshotTx`
Expected: FAIL to compile: `undefined: storage.InTenantSnapshotTx`.

- [ ] **Step 3: Implement `InTenantSnapshotTx`**

In `internal/storage/storage.go`, after `InTenantReadTx`:

```go
// InTenantSnapshotTx is InTenantTx at REPEATABLE READ: every statement in fn
// sees the snapshot of the first, and fn may still write. A change-set plan
// (ADR-026) uses it so that what it reads, and the digest PostgreSQL computes
// over those rows before it commits, describe the same registry.
func InTenantSnapshotTx(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(pgx.Tx) error) error {
	id, err := uuid.Parse(tenantID)
	if err != nil || id == uuid.Nil {
		return fmt.Errorf("storage: invalid tenant id %q", tenantID)
	}
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead}
	return pgx.BeginTxFunc(ctx, pool, opts, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, id.String()); err != nil {
			return fmt.Errorf("storage: set tenant context: %w", err)
		}
		return fn(tx)
	})
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -race -count=1 ./internal/storage/ -run TestInTenantSnapshotTx`
Expected: PASS.

- [ ] **Step 5: Create `registry.Tx` by moving the SQL out of the Service methods**

`internal/registry/tx.go`:

```go
package registry

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tx makes registry writes in a caller's transaction, which must already
// have its tenant and actor set (storage.InTenantTx, storage.SetActor).
// Service uses it for its one-transaction changes, and a bundle change set
// (ADR-026) uses it so that each step is exactly the write the API makes:
// the registry triggers decide both the same way. Errors are the raw
// database errors; map them with MapErr.
type Tx struct{ pgx.Tx }

// MapErr maps a registry database error to the error kinds of this package.
func MapErr(err error) error { return mapErr(err) }

// RegisterConnector inserts an immutable connector.
func (t Tx) RegisterConnector(ctx context.Context, n NewConnector) (Connector, error) {
	c := Connector{Name: n.Name, Protocol: n.Protocol, Endpoint: n.Endpoint, SecretRef: n.SecretRef, Tools: []string{}}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.connectors (tenant_id, name, protocol, endpoint, secret_ref)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		n.Name, n.Protocol, n.Endpoint, n.SecretRef).Scan(&c.ID)
	return c, err
}

// RegisterTool inserts a tool on a connector.
func (t Tx) RegisterTool(ctx context.Context, connectorID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.tools (tenant_id, connector_id, name)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, connectorID, name).Scan(&id)
	return id, err
}

// ProposeContract inserts an immutable contract version for a tool.
func (t Tx) ProposeContract(ctx context.Context, toolID uuid.UUID, c Contract) (uuid.UUID, error) {
	noEffect := c.NoEffectErrors
	if noEffect == nil {
		noEffect = []string{}
	}
	var id uuid.UUID
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.tool_contracts
		    (tenant_id, tool_id, side_effects, idempotency_mode, idempotency_key_field, correlation_field,
		     reconciliation_lookup, reconciliation_consistency, proof_standard, no_effect_errors,
		     max_attempts, timeout_ms, concurrency_group, max_inflight, data_sensitivity, schedule_priority,
		     cost_unit, cost_fixed, cost_amount_field, cost_unit_field,
		     max_queued, retry_max_elapsed_ms, retry_max_cost, definition_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, COALESCE($17::numeric, 0), $18, $19, $20, $21, $22::numeric, $23)
		RETURNING id`,
		toolID, c.SideEffects, c.IdempotencyMode, nullStr(c.IdempotencyKeyField), nullStr(c.CorrelationField),
		c.ReconciliationLookup, c.ReconciliationConsistency, c.ProofStandard, noEffect,
		c.MaxAttempts, nullInt(c.TimeoutMS), nullStr(c.ConcurrencyGroup), nullInt(c.MaxInflight),
		nullStr(c.DataSensitivity), c.SchedulePriority, nullStr(c.CostUnit), nullStr(string(c.CostFixed)),
		nullStr(c.CostAmountField), nullStr(c.CostUnitField),
		nullInt(c.MaxQueued), nullInt(c.RetryMaxElapsedMS), nullStr(string(c.RetryMaxCost)), c.DefinitionID).Scan(&id)
	return id, err
}

// ActivateContract makes a contract the tool's active one (two-person).
func (t Tx) ActivateContract(ctx context.Context, toolID, contractID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.tools SET active_contract_id = $2 WHERE id = $1`, toolID, contractID)
}

// RevokeContract permanently revokes a contract.
func (t Tx) RevokeContract(ctx context.Context, contractID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.tool_contracts SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`,
		contractID, reason)
}

// RegisterAgent inserts an immutable agent.
func (t Tx) RegisterAgent(ctx context.Context, n NewAgent) (Agent, error) {
	return scanAgent(t.QueryRow(ctx, `
		INSERT INTO eacp.agents (tenant_id, name, display_name, environment, risk_class, owner_principal_id, owner_group_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6)
		RETURNING `+agentColumns,
		n.Name, n.DisplayName, n.Environment, n.RiskClass, nullID(n.OwnerPrincipalID), nullID(n.OwnerGroupID)))
}

// RegisterVersion inserts a REGISTERED version (the database numbers it).
func (t Tx) RegisterVersion(ctx context.Context, agentID uuid.UUID, n NewVersion) (Version, error) {
	v := Version{AgentID: agentID, Runtime: n.Runtime, CodeRef: n.CodeRef, AllowedTools: []string{}}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.agent_versions (tenant_id, agent_id, runtime, code_ref)
		VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id, version, state`,
		agentID, n.Runtime, n.CodeRef).Scan(&v.ID, &v.Number, &v.State)
	return v, err
}

// ProposeAllowlist inserts an immutable allowlist of "connector.tool" refs.
func (t Tx) ProposeAllowlist(ctx context.Context, versionID uuid.UUID, tools []string) (uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(tools))
	for _, ref := range tools {
		toolID, err := resolveTool(ctx, t.Tx, ref)
		if err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, toolID)
	}
	var id uuid.UUID
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.agent_allowlists (tenant_id, agent_version_id, tool_ids)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, versionID, ids).Scan(&id)
	return id, err
}

// ActivateAllowlist points a version at an allowlist (two-person).
func (t Tx) ActivateAllowlist(ctx context.Context, versionID, allowlistID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.agent_versions SET active_allowlist_id = $2 WHERE id = $1`,
		versionID, allowlistID)
}

// TransitionVersion moves a version through its lifecycle (ADR-003 §2).
func (t Tx) TransitionVersion(ctx context.Context, versionID uuid.UUID, to State, reason string) error {
	var from State
	if err := t.QueryRow(ctx, `SELECT state FROM eacp.agent_versions WHERE id = $1`, versionID).Scan(&from); err != nil {
		return err
	}
	if from == to {
		return newErr(ErrConflict, "version is already %s", to)
	}
	return execOne(ctx, t.Tx, `UPDATE eacp.agent_versions SET state = $2, state_reason = $3 WHERE id = $1`,
		versionID, string(to), reason)
}
```

Replace each Service method body with a call to `Tx`. For example, in `internal/registry/connectors.go`:

```go
// RegisterConnector registers an immutable connector (registry_editor).
func (s *Service) RegisterConnector(ctx context.Context, a Actor, n NewConnector) (Connector, error) {
	var c Connector
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.RegisterConnector(ctx, n)
		return err
	})
	return c, err
}

// RegisterTool registers a tool on a connector. It cannot execute until a
// contract is activated for it.
func (s *Service) RegisterTool(ctx context.Context, a Actor, connectorID uuid.UUID, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.RegisterTool(ctx, connectorID, name)
		return err
	})
	return id, err
}

// ProposeContract records an immutable contract version for a tool.
func (s *Service) ProposeContract(ctx context.Context, a Actor, toolID uuid.UUID, c Contract) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.ProposeContract(ctx, toolID, c)
		return err
	})
	return id, err
}

// ActivateContract makes a contract the tool's active one (two-person).
func (s *Service) ActivateContract(ctx context.Context, a Actor, toolID, contractID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.ActivateContract(ctx, toolID, contractID) })
}

// RevokeContract permanently revokes a contract; its tool stops being
// executable until another contract is activated.
func (s *Service) RevokeContract(ctx context.Context, a Actor, contractID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.RevokeContract(ctx, contractID, reason) })
}
```

and in `internal/registry/agents.go`:

```go
// RegisterAgent registers an agent (registry_editor).
func (s *Service) RegisterAgent(ctx context.Context, a Actor, n NewAgent) (Agent, error) {
	var ag Agent
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		ag, err = Tx{tx}.RegisterAgent(ctx, n)
		return err
	})
	return ag, err
}

// RegisterVersion registers a new version (number assigned by the database)
// in state REGISTERED.
func (s *Service) RegisterVersion(ctx context.Context, a Actor, agentID uuid.UUID, n NewVersion) (Version, error) {
	var v Version
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		v, err = Tx{tx}.RegisterVersion(ctx, agentID, n)
		return err
	})
	return v, err
}

// ProposeAllowlist records an immutable allowlist of "connector.tool" refs for
// a version. A second person activates it.
func (s *Service) ProposeAllowlist(ctx context.Context, a Actor, versionID uuid.UUID, tools []string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.ProposeAllowlist(ctx, versionID, tools)
		return err
	})
	return id, err
}

// ActivateAllowlist points a version at an allowlist (two-person).
func (s *Service) ActivateAllowlist(ctx context.Context, a Actor, versionID, allowlistID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.ActivateAllowlist(ctx, versionID, allowlistID) })
}

// TransitionVersion moves a version through its lifecycle (ADR-003 §2).
func (s *Service) TransitionVersion(ctx context.Context, a Actor, versionID uuid.UUID, to State, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.TransitionVersion(ctx, versionID, to, reason) })
}
```

`RegisterVersion` used to return a version with its fields set even on error. No caller relies on that; check with `grep -rn "RegisterVersion(" --include=*.go`.

- [ ] **Step 6: Run the registry and API tests to verify nothing changed**

Run: `go vet ./... && go test -race -count=1 ./internal/registry/... ./internal/api/ ./internal/storage/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/storage/storage.go internal/storage/storage_test.go internal/registry/tx.go internal/registry/agents.go internal/registry/connectors.go
git commit -m "refactor(registry): share tx-level writes; add a snapshot tenant transaction"
```

---

### Task 3: The desired document: types, strict decoding, validation

**Files:**
- Create: `internal/bundle/document.go`
- Test: `internal/bundle/document_test.go`

**Interfaces:**
- Consumes: `registry.Contract`
- Produces: `type Document`, `Connector`, `Tool`, `Agent`, `Owner`, `VersionSpec`, `Import`; `Decode(raw json.RawMessage) (Document, error)`; `Validate(d Document, raw json.RawMessage) []Finding`; `type Finding{Address, Kind, Detail string}` with `Blocking() bool`; the `Kind*` constants; `blocked([]Finding) bool`; `slugRE`, `toolNameRE`; `sortedKeys[V any](map[string]V) []string`; `MaxSteps = 2000`.

- [ ] **Step 1: Write the failing tests**

`internal/bundle/document_test.go`:

```go
package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

const validDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func findingKinds(fs []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		out[f.Address] = f.Kind
	}
	return out
}

func TestAValidDocumentHasNoFindings(t *testing.T) {
	d, err := Decode(json.RawMessage(validDoc))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(validDoc)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestDecodeRefusesUnknownFieldsSoNoSecretIsCarried(t *testing.T) {
	for _, raw := range []string{
		`{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://x", "secret_ref": "l", "token": "s3cr3t"}}}`,
		`{"agents": {"a1": {"password": "x"}}}`,
		`{"secrets": {}}`,
		`{"connectors": {}} {"trailing": 1}`,
		``,
	} {
		if _, err := Decode(json.RawMessage(raw)); err == nil {
			t.Errorf("Decode(%s) accepted it", raw)
		}
	}
}

func TestValidateReportsEveryStructuralProblem(t *testing.T) {
	raw := `{
	 "connectors": {"Bad_Name": {"protocol": "ftp", "endpoint": "ftp://x", "secret_ref": "has space",
	   "tools": {"t": {"contract": {"definition_id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"}}}}},
	 "agents": {"bot": {"display_name": " ", "environment": "prod", "risk_class": "extreme",
	   "owner": {"principal": "carol", "group": "team"}, "version": {"runtime": "", "code_ref": ""},
	   "allowlist": ["no-dot", "a.b", "a.b"], "state": "RETIRED"},
	  "idle": {"display_name": "Idle", "environment": "staging", "risk_class": "low", "owner": {"group": "team"},
	   "version": {"runtime": "go", "code_ref": "git:1"}, "state": "ACTIVE"}},
	 "imports": [{"to": "agent.missing", "id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"},
	   {"to": "allowlist.bot", "id": "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"}]}`
	raw = strings.Replace(raw, `"code_ref": "git:1"`, `"code_ref": "${var.ref}"`, 1)
	d, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	got := findingKinds(Validate(d, json.RawMessage(raw)))
	for addr, kind := range map[string]string{
		"bundle":                KindUnresolvedVariable,
		"connector.Bad_Name":    KindInvalid,
		"contract.Bad_Name.t":   KindInvalid,
		"agent.bot":             KindInvalid,
		"version.bot":           KindInvalid,
		"allowlist.bot":         KindInvalid,
		"version.idle":          KindInvalid, // ACTIVE without an allowlist
		"import.agent.missing":  KindInvalid,
		"import.allowlist.bot":  KindInvalid,
	} {
		if got[addr] != kind {
			t.Errorf("%s: finding %q, want %q (all: %v)", addr, got[addr], kind, got)
		}
	}
}

func TestOnlyOrphansAndUnmanagedReferencesDoNotBlock(t *testing.T) {
	for kind, want := range map[string]bool{
		KindInvalid: true, KindUnresolvedVariable: true, KindUnresolvedReference: true, KindUnsupported: true,
		KindUnmanaged: true, KindRequiresRelease: true, KindContained: true,
		KindOrphan: false, KindUnmanagedReference: false,
	} {
		if (Finding{Kind: kind}).Blocking() != want {
			t.Errorf("%s blocking = %v", kind, !want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Document|Decode|Validate|Orphans'`
Expected: FAIL to compile: `undefined: Decode`.

- [ ] **Step 3: Implement `document.go`**

`internal/bundle/document.go`:

```go
package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"eacp/internal/registry"
)

// MaxSteps is the most steps one change set may have (the ordinal CHECK of
// migration 00020).
const MaxSteps = 2000

// Document is a bundle's resolved desired state. eacpctl resolves targets
// and variables before sending it; the server accepts JSON only.
type Document struct {
	Connectors map[string]Connector `json:"connectors,omitempty"`
	Agents     map[string]Agent     `json:"agents,omitempty"`
	Imports    []Import             `json:"imports,omitempty"`
}

// Connector is a declared connector. SecretRef names a secret the worker
// holds; it is never a secret value.
type Connector struct {
	Protocol  string          `json:"protocol"`
	Endpoint  string          `json:"endpoint"`
	SecretRef string          `json:"secret_ref"`
	Tools     map[string]Tool `json:"tools,omitempty"`
}

// Tool is a declared tool. A tool of an MCP connector is discovered, never
// created; declaring it only manages its contract.
type Tool struct {
	Contract *registry.Contract `json:"contract,omitempty"`
}

// Owner names the agent's one owner: a principal or a group.
type Owner struct {
	Principal string `json:"principal,omitempty"`
	Group     string `json:"group,omitempty"`
}

// VersionSpec identifies the version the bundle wants.
type VersionSpec struct {
	Runtime string `json:"runtime"`
	CodeRef string `json:"code_ref"`
}

// Agent is a declared agent. State "ACTIVE" asks for the version to be
// activated; omitted, the bundle leaves the version's state alone.
type Agent struct {
	DisplayName string      `json:"display_name"`
	Environment string      `json:"environment"`
	RiskClass   string      `json:"risk_class"`
	Owner       Owner       `json:"owner"`
	Version     VersionSpec `json:"version"`
	Allowlist   []string    `json:"allowlist,omitempty"`
	State       string      `json:"state,omitempty"`
}

// Import adopts an existing object into the bundle without changing it.
type Import struct {
	To string    `json:"to"`
	ID uuid.UUID `json:"id"`
}

// Finding kinds. Orphans and unmanaged references are reported; every other
// kind blocks the plan.
const (
	KindInvalid             = "invalid"
	KindUnresolvedVariable  = "unresolved_variable"
	KindUnresolvedReference = "unresolved_reference"
	KindUnsupported         = "unsupported"
	KindUnmanaged           = "unmanaged"
	KindRequiresRelease     = "requires_release"
	KindContained           = "contained"
	KindOrphan              = "orphan"
	KindUnmanagedReference  = "unmanaged_reference"
)

// Finding is something a plan reports about an address.
type Finding struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
}

// Blocking reports whether the finding stops the plan from being recorded.
func (f Finding) Blocking() bool { return f.Kind != KindOrphan && f.Kind != KindUnmanagedReference }

func blocked(fs []Finding) bool { return slices.ContainsFunc(fs, Finding.Blocking) }

var (
	slugRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	toolNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	secretRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)
	endpointRE = regexp.MustCompile(`^https?://[^[:space:]]+$`)
	importRE   = regexp.MustCompile(`^(connector|tool|agent|version)\.`)
)

// Decode strictly decodes a desired document: an unknown field (such as a
// secret someone tried to put in a bundle) is refused, not dropped.
func Decode(raw json.RawMessage) (Document, error) {
	var d Document
	if len(bytes.TrimSpace(raw)) == 0 {
		return d, errors.New("desired: required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("desired: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return d, errors.New("desired: trailing data after the document")
	}
	return d, nil
}

// Validate reports every structural problem of d as a blocking finding. The
// registry triggers check contract fields again when a step runs.
func Validate(d Document, raw json.RawMessage) []Finding {
	var out []Finding
	bad := func(addr, format string, args ...any) {
		out = append(out, Finding{Address: addr, Kind: KindInvalid, Detail: fmt.Sprintf(format, args...)})
	}
	if bytes.Contains(raw, []byte("${")) {
		out = append(out, Finding{Address: "bundle", Kind: KindUnresolvedVariable,
			Detail: "a ${...} reference was not resolved: resolve variables with eacpctl before planning"})
	}
	for _, name := range sortedKeys(d.Connectors) {
		c, addr := d.Connectors[name], "connector."+name
		if !slugRE.MatchString(name) {
			bad(addr, "connector names match %s", slugRE)
		}
		if c.Protocol != "http" && c.Protocol != "mcp" {
			bad(addr, "protocol is http or mcp")
		}
		if !endpointRE.MatchString(c.Endpoint) || len(c.Endpoint) > 2048 {
			bad(addr, "endpoint is an http(s) URL")
		}
		if !secretRE.MatchString(c.SecretRef) {
			bad(addr, "secret_ref names a secret the worker holds; it is never a secret value")
		}
		for _, tn := range sortedKeys(c.Tools) {
			if !toolNameRE.MatchString(tn) {
				bad("tool."+name+"."+tn, "tool names match %s", toolNameRE)
			}
			if ct := c.Tools[tn].Contract; ct != nil && ct.DefinitionID != nil {
				bad("contract."+name+"."+tn, "definition_id is pinned by the planner, never declared")
			}
		}
	}
	for _, name := range sortedKeys(d.Agents) {
		a, addr, vaddr := d.Agents[name], "agent."+name, "version."+name
		if !slugRE.MatchString(name) {
			bad(addr, "agent names match %s", slugRE)
		}
		if strings.TrimSpace(a.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		if !slices.Contains([]string{"development", "staging", "production"}, a.Environment) {
			bad(addr, "environment is development, staging or production")
		}
		if !slices.Contains([]string{"low", "medium", "high", "critical"}, a.RiskClass) {
			bad(addr, "risk_class is low, medium, high or critical")
		}
		if (a.Owner.Principal == "") == (a.Owner.Group == "") {
			bad(addr, "owner names exactly one principal or group")
		}
		if strings.TrimSpace(a.Version.Runtime) == "" || strings.TrimSpace(a.Version.CodeRef) == "" {
			bad(vaddr, "version needs runtime and code_ref")
		}
		if a.State != "" && a.State != string(registry.StateActive) {
			bad(vaddr, "state is ACTIVE or omitted")
		}
		if a.State == string(registry.StateActive) && len(a.Allowlist) == 0 {
			bad(vaddr, "an ACTIVE version needs an allowlist")
		}
		seen := map[string]bool{}
		for _, ref := range a.Allowlist {
			conn, tool, ok := strings.Cut(ref, ".")
			if !ok || conn == "" || tool == "" || strings.Contains(tool, ".") {
				bad("allowlist."+name, "%q is not connector.tool", ref)
			}
			if seen[ref] {
				bad("allowlist."+name, "%q is listed twice", ref)
			}
			seen[ref] = true
		}
	}
	seen := map[string]bool{}
	for _, im := range d.Imports {
		addr := "import." + im.To
		switch {
		case !importRE.MatchString(im.To):
			bad(addr, "imports adopt a connector, tool, agent or version")
		case !declared(d, im.To):
			bad(addr, "%s is not declared in this bundle", im.To)
		case im.ID == uuid.Nil:
			bad(addr, "id is required")
		case seen[im.To]:
			bad(addr, "%s is imported twice", im.To)
		}
		seen[im.To] = true
	}
	return out
}

// declared reports whether the document declares addr.
func declared(d Document, addr string) bool {
	kind, name, _ := strings.Cut(addr, ".")
	switch kind {
	case "connector":
		_, ok := d.Connectors[name]
		return ok
	case "tool", "contract":
		conn, tool, _ := strings.Cut(name, ".")
		_, ok := d.Connectors[conn].Tools[tool]
		return ok
	case "agent", "version", "allowlist":
		_, ok := d.Agents[name]
		return ok
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Document|Decode|Validate|Orphans'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bundle/document.go internal/bundle/document_test.go
git commit -m "feat(bundle): strict desired document and validation"
```

---

### Task 4: Registry state and the pure diff

**Files:**
- Create: `internal/bundle/state.go`
- Create: `internal/bundle/plan.go`
- Test: `internal/bundle/plan_test.go`

**Interfaces:**
- Consumes: `Document`, `Finding`, the `Kind*` constants, `sortedKeys`, `MaxSteps` (Task 3); `registry.Contract`, `registry.NewConnector`, `registry.NewAgent`, `registry.NewVersion`, `registry.State*`.
- Produces: `type State`, `ConnectorState`, `ToolState`, `AgentState`, `VersionState`; `loadState(ctx, tx pgx.Tx, bundle string) (State, error)`; `type Step{Ordinal int; Address, Op, Stage string; Payload Payload; ObjectID *uuid.UUID}`; `type Payload` (fields below); `type Ref{Kind string; ID uuid.UUID}`; `type Diff{Steps []Step; Findings []Finding; Refs []Ref}`; `diff(bundle string, changeSet uuid.UUID, doc Document, st State, prune bool) Diff`; constants `OpCreate, OpPropose, OpActivate, OpTransition, OpRevoke, OpImport`, `StageSubmit, StageApprove`; `sameContract(a, b registry.Contract) bool`.

- [ ] **Step 1: Write the failing diff tests**

`internal/bundle/plan_test.go`:

```go
package bundle

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"eacp/internal/registry"
)

var (
	carol   = uuid.MustParse("00000000-0000-4000-8000-00000000ca01")
	cs      = uuid.MustParse("00000000-0000-4000-8000-0000000000c5")
	connID  = uuid.MustParse("00000000-0000-4000-8000-000000000c01")
	toolID  = uuid.MustParse("00000000-0000-4000-8000-000000000701")
	ctID    = uuid.MustParse("00000000-0000-4000-8000-000000000c71")
	agentID = uuid.MustParse("00000000-0000-4000-8000-000000000a01")
	v1ID    = uuid.MustParse("00000000-0000-4000-8000-000000000b01")
	v2ID    = uuid.MustParse("00000000-0000-4000-8000-000000000b02")
	defID   = uuid.MustParse("00000000-0000-4000-8000-000000000d01")
)

func readOnly() registry.Contract {
	return registry.Contract{SideEffects: []string{"READ_ONLY"}, IdempotencyMode: "none",
		ReconciliationLookup: "none", ReconciliationConsistency: "none", ProofStandard: "none", MaxAttempts: 3}
}

func mustDoc(t *testing.T, raw string) Document {
	t.Helper()
	d, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(raw)); len(fs) != 0 {
		t.Fatalf("invalid test document: %+v", fs)
	}
	return d
}

func empty() State {
	return State{Managed: map[string]uuid.UUID{}, ManagedElsewhere: map[uuid.UUID]string{},
		Connectors: map[string]ConnectorState{}, Agents: map[string]AgentState{},
		Principals: map[string]uuid.UUID{"carol": carol}, Groups: map[string]uuid.UUID{}}
}

// applied is the state after the validDoc bundle "ledger" was applied.
func applied() State {
	st := empty()
	c := readOnly()
	st.Connectors["ledger"] = ConnectorState{ID: connID, Protocol: "http", Endpoint: "http://fakeerp:8090",
		SecretRef: "ledger", Tools: map[string]ToolState{"post_entry": {ID: toolID, ActiveContractID: &ctID,
			ActiveContract: &c}}}
	st.Agents["ledger-bot"] = AgentState{ID: agentID, DisplayName: "Ledger bot", Environment: "production",
		RiskClass: "high", OwnerPrincipalID: carol, Versions: []VersionState{{ID: v1ID, Number: 1,
			Runtime: "python", CodeRef: "git:aaa111", State: registry.StateActive,
			AllowedTools: []string{"ledger.post_entry"}}}}
	for addr, id := range map[string]uuid.UUID{"connector.ledger": connID, "tool.ledger.post_entry": toolID,
		"agent.ledger-bot": agentID, "version.ledger-bot": v1ID} {
		st.Managed[addr] = id
	}
	return st
}

func ops(d Diff) []string {
	out := make([]string, 0, len(d.Steps))
	for _, s := range d.Steps {
		out = append(out, fmt.Sprintf("%d %s %s %s", s.Ordinal, s.Stage, s.Op, s.Address))
	}
	return out
}

func wantOps(t *testing.T, d Diff, want ...string) {
	t.Helper()
	if got := ops(d); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps:\n%s\nwant:\n%s\nfindings: %+v", strings.Join(got, "\n"), strings.Join(want, "\n"), d.Findings)
	}
}

func wantFinding(t *testing.T, d Diff, addr, kind string) {
	t.Helper()
	for _, f := range d.Findings {
		if f.Address == addr && f.Kind == kind {
			return
		}
	}
	t.Fatalf("no %s finding at %s: %+v", kind, addr, d.Findings)
}

func TestDiffCreatesEverythingInDependencyOrder(t *testing.T) {
	d := diff("ledger", cs, mustDoc(t, validDoc), empty(), false)
	wantOps(t, d,
		"1 submit create connector.ledger",
		"2 submit create tool.ledger.post_entry",
		"3 submit propose contract.ledger.post_entry",
		"4 submit create agent.ledger-bot",
		"5 submit create version.ledger-bot",
		"6 submit propose allowlist.ledger-bot",
		"7 approve activate contract.ledger.post_entry",
		"8 approve activate allowlist.ledger-bot",
		"9 approve transition version.ledger-bot")
	if p := d.Steps[1].Payload; p.Parent != "connector.ledger" || p.ParentID != nil {
		t.Fatalf("tool parent = %+v", p)
	}
	if p := d.Steps[6].Payload; p.ProposalStep != 3 {
		t.Fatalf("contract activation proposal = %d", p.ProposalStep)
	}
	if p := d.Steps[8].Payload; p.To != registry.StateActive || !strings.Contains(p.Reason, cs.String()) {
		t.Fatalf("transition = %+v", p)
	}
	if p := d.Steps[3].Payload; p.Agent == nil || p.Agent.OwnerPrincipalID != carol {
		t.Fatalf("agent payload = %+v", p)
	}
	if len(d.Findings) != 0 {
		t.Fatalf("findings = %+v", d.Findings)
	}
}

func TestDiffOfTheAppliedStateIsEmpty(t *testing.T) {
	d := diff("ledger", cs, mustDoc(t, validDoc), applied(), false)
	wantOps(t, d)
	if len(d.Findings) != 0 || len(d.Refs) == 0 {
		t.Fatalf("findings %+v, refs %v", d.Findings, d.Refs)
	}
}

func TestContractNumbersAndAllowlistOrderDoNotDrift(t *testing.T) {
	st := applied()
	c := readOnly()
	c.CostFixed = "0.000000"
	c.NoEffectErrors = []string{}
	tl := st.Connectors["ledger"].Tools["post_entry"]
	tl.ActiveContract = &c
	st.Connectors["ledger"].Tools["post_entry"] = tl
	raw := strings.Replace(validDoc, `"max_attempts": 3`, `"max_attempts": 3, "cost_fixed": 0`, 1)
	wantOps(t, diff("ledger", cs, mustDoc(t, raw), st, false))
	if !sameContract(registry.Contract{CostFixed: "5", RetryMaxCost: "1.50"},
		registry.Contract{CostFixed: "5.000000", RetryMaxCost: "1.5"}) {
		t.Fatal("equal numbers compare unequal")
	}
}

func TestDiffProposesChangedContractsAndAllowlists(t *testing.T) {
	raw := strings.Replace(validDoc, `"max_attempts": 3`, `"max_attempts": 2`, 1)
	wantOps(t, diff("ledger", cs, mustDoc(t, raw), applied(), false),
		"1 submit propose contract.ledger.post_entry",
		"2 approve activate contract.ledger.post_entry")

	st := applied()
	st.Connectors["ledger"].Tools["audit"] = ToolState{ID: uuid.New()}
	raw = strings.Replace(validDoc, `"tools": {`, `"tools": {"audit": {}, `, 1)
	raw = strings.Replace(raw, `["ledger.post_entry"]`, `["ledger.post_entry", "ledger.audit"]`, 1)
	st.Managed["tool.ledger.audit"] = st.Connectors["ledger"].Tools["audit"].ID
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d,
		"1 submit propose allowlist.ledger-bot",
		"2 approve activate allowlist.ledger-bot")
	if got := d.Steps[0].Payload.Tools; strings.Join(got, ",") != "ledger.audit,ledger.post_entry" {
		t.Fatalf("allowlist = %v", got)
	}
}

func TestANewCodeRefOfALiveAgentRequiresARelease(t *testing.T) {
	raw := strings.Replace(validDoc, "git:aaa111", "git:bbb222", 1)
	d := diff("ledger", cs, mustDoc(t, raw), applied(), false)
	wantFinding(t, d, "version.ledger-bot", KindRequiresRelease)

	// Without state ACTIVE the bundle registers the new version; the old
	// ACTIVE one is reported, never retired, even with prune.
	raw = strings.Replace(raw, `, "state": "ACTIVE"`, "", 1)
	d = diff("ledger", cs, mustDoc(t, raw), applied(), true)
	wantOps(t, d,
		"1 submit create version.ledger-bot",
		"2 submit propose allowlist.ledger-bot",
		"3 approve activate allowlist.ledger-bot")
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
}

func TestDiffAdoptsAPromotedVersionAndRetiresTheOldOneOnPrune(t *testing.T) {
	st := applied()
	ag := st.Agents["ledger-bot"]
	// A release promoted v2 (git:bbb222) and suspended v1.
	ag.Versions = []VersionState{
		{ID: v2ID, Number: 2, Runtime: "python", CodeRef: "git:bbb222", State: registry.StateActive,
			AllowedTools: []string{"ledger.post_entry"}},
		{ID: v1ID, Number: 1, Runtime: "python", CodeRef: "git:aaa111", State: registry.StateSuspended,
			AllowedTools: []string{"ledger.post_entry"}},
	}
	st.Agents["ledger-bot"] = ag
	raw := strings.Replace(validDoc, "git:aaa111", "git:bbb222", 1)
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit import version.ledger-bot")
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
	if d.Steps[0].Payload.ObjectID == nil || *d.Steps[0].Payload.ObjectID != v2ID {
		t.Fatalf("import = %+v", d.Steps[0].Payload)
	}
	d = diff("ledger", cs, mustDoc(t, raw), st, true)
	wantOps(t, d, "1 submit import version.ledger-bot", "2 approve transition version.ledger-bot")
	if p := d.Steps[1].Payload; p.To != registry.StateRetired || p.ParentID == nil || *p.ParentID != v1ID {
		t.Fatalf("retire = %+v", p)
	}
}

func TestDiffNeverUndoesContainmentOrChangesImmutableFields(t *testing.T) {
	st := applied()
	ag := st.Agents["ledger-bot"]
	ag.Versions[0].State = registry.StateSuspended
	st.Agents["ledger-bot"] = ag
	wantFinding(t, diff("ledger", cs, mustDoc(t, validDoc), st, false), "version.ledger-bot", KindContained)

	raw := strings.Replace(validDoc, "http://fakeerp:8090", "http://other:1", 1)
	raw = strings.Replace(raw, `"risk_class": "high"`, `"risk_class": "low"`, 1)
	d := diff("ledger", cs, mustDoc(t, raw), applied(), false)
	wantFinding(t, d, "connector.ledger", KindUnsupported)
	wantFinding(t, d, "agent.ledger-bot", KindUnsupported)
}

func TestExistingObjectsMustBeImportedAndBelongToOneBundle(t *testing.T) {
	st := applied()
	st.Managed = map[string]uuid.UUID{}
	d := diff("ledger", cs, mustDoc(t, validDoc), st, false)
	wantFinding(t, d, "connector.ledger", KindUnmanaged)
	wantFinding(t, d, "agent.ledger-bot", KindUnmanaged)

	raw := strings.Replace(validDoc, `"agents"`, fmt.Sprintf(`"imports": [{"to": "connector.ledger", "id": "%s"},
		{"to": "agent.ledger-bot", "id": "%s"}], "agents"`, connID, agentID), 1)
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit import connector.ledger", "2 submit import agent.ledger-bot",
		"3 submit import version.ledger-bot")

	st.ManagedElsewhere[connID] = "finance"
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantFinding(t, d, "connector.ledger", KindUnmanaged)

	raw = strings.Replace(raw, connID.String(), uuid.NewString(), 1)
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), applied(), false), "connector.ledger", KindUnresolvedReference)
}

func TestMCPToolsAreDiscoveredAndPinTheirDefinition(t *testing.T) {
	raw := strings.Replace(validDoc, `"protocol": "http"`, `"protocol": "mcp"`, 1)
	d := diff("ledger", cs, mustDoc(t, raw), empty(), false)
	wantFinding(t, d, "tool.ledger.post_entry", KindUnresolvedReference)

	st := applied()
	c := st.Connectors["ledger"]
	c.Protocol = "mcp"
	tl := c.Tools["post_entry"]
	tl.DefinitionID = &defID
	c.Tools["post_entry"] = tl
	st.Connectors["ledger"] = c
	d = diff("ledger", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit propose contract.ledger.post_entry", "2 approve activate contract.ledger.post_entry")
	if p := d.Steps[0].Payload.Contract; p.DefinitionID == nil || *p.DefinitionID != defID {
		t.Fatalf("contract does not pin the definition: %+v", p)
	}
	tl.Quarantined = true
	c.Tools["post_entry"] = tl
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), st, false), "contract.ledger.post_entry", KindContained)
}

func TestReferencesAndOrphans(t *testing.T) {
	raw := strings.Replace(validDoc, `["ledger.post_entry"]`, `["ledger.post_entry", "erp.po", "nope.x"]`, 1)
	st := applied()
	st.Connectors["erp"] = ConnectorState{ID: uuid.New(), Protocol: "http", Tools: map[string]ToolState{
		"po": {ID: uuid.New()}}}
	d := diff("ledger", cs, mustDoc(t, raw), st, false)
	wantFinding(t, d, "tool.erp.po", KindUnmanagedReference)
	wantFinding(t, d, "allowlist.ledger-bot", KindUnresolvedReference)

	raw = strings.Replace(validDoc, `"principal": "carol"`, `"principal": "nobody"`, 1)
	wantFinding(t, diff("ledger", cs, mustDoc(t, raw), empty(), false), "agent.ledger-bot", KindUnresolvedReference)

	// Dropping the tool and the agent: orphans, and with prune the tool's
	// contract is revoked. The ACTIVE version is never retired by prune.
	noAgent := `{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger"}}}`
	d = diff("ledger", cs, mustDoc(t, noAgent), applied(), false)
	wantOps(t, d)
	wantFinding(t, d, "tool.ledger.post_entry", KindOrphan)
	wantFinding(t, d, "agent.ledger-bot", KindOrphan)
	wantFinding(t, d, "version.ledger-bot", KindOrphan)
	d = diff("ledger", cs, mustDoc(t, noAgent), applied(), true)
	wantOps(t, d, "1 approve revoke contract.ledger.post_entry")
	if p := d.Steps[0].Payload; p.ObjectID == nil || *p.ObjectID != ctID {
		t.Fatalf("revoke = %+v", p)
	}
}

func TestPlansAreBoundedTo2000Steps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"connectors": {`)
	for i := range 700 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"c%03d": {"protocol": "http", "endpoint": "http://x", "secret_ref": "s",
			"tools": {"t": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
			"reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
			"max_attempts": 1}}}}`, i)
	}
	b.WriteString(`}}`)
	d := diff("big", cs, mustDoc(t, b.String()), empty(), false)
	wantFinding(t, d, "bundle", KindInvalid)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Diff|Contract|Release|Adopt|Containment|Import|MCP|References|Bounded'`
Expected: FAIL to compile: `undefined: diff`.

- [ ] **Step 3: Implement `state.go`**

`internal/bundle/state.go`:

```go
package bundle

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// State is the registry as one plan sees it, read in one snapshot.
type State struct {
	Managed          map[string]uuid.UUID      // this bundle's addresses
	ManagedElsewhere map[uuid.UUID]string      // object -> the other bundle managing it
	Connectors       map[string]ConnectorState // by name
	Agents           map[string]AgentState     // by name
	Principals       map[string]uuid.UUID      // enabled principals by name
	Groups           map[string]uuid.UUID      // by name
}

// ConnectorState is a registered connector and its tools by name.
type ConnectorState struct {
	ID                            uuid.UUID
	Protocol, Endpoint, SecretRef string
	Tools                         map[string]ToolState
}

// ToolState is a tool and its active contract. ActiveContract is nil when
// the tool has none or it is revoked.
type ToolState struct {
	ID               uuid.UUID
	DefinitionID     *uuid.UUID
	Quarantined      bool
	ActiveContractID *uuid.UUID
	ActiveContract   *registry.Contract
}

// AgentState is an agent and its versions, newest first.
type AgentState struct {
	ID                                   uuid.UUID
	DisplayName, Environment, RiskClass  string
	OwnerPrincipalID, OwnerGroupID       uuid.UUID
	Versions                             []VersionState
}

// VersionState is a version and its active allowlist, sorted.
type VersionState struct {
	ID               uuid.UUID
	Number           int
	Runtime, CodeRef string
	State            registry.State
	AllowedTools     []string
}

func loadState(ctx context.Context, tx pgx.Tx, bundle string) (State, error) {
	st := State{Managed: map[string]uuid.UUID{}, ManagedElsewhere: map[uuid.UUID]string{},
		Connectors: map[string]ConnectorState{}, Agents: map[string]AgentState{},
		Principals: map[string]uuid.UUID{}, Groups: map[string]uuid.UUID{}}

	rows, err := tx.Query(ctx, `SELECT br.address, br.object_id, b.name FROM eacp.bundle_resources br
		JOIN eacp.bundles b ON b.tenant_id = br.tenant_id AND b.id = br.bundle_id`)
	if err != nil {
		return st, err
	}
	var addr, owner string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&addr, &id, &owner}, func() error {
		if owner == bundle {
			st.Managed[addr] = id
		} else {
			st.ManagedElsewhere[id] = owner
		}
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT id, name, protocol, endpoint, secret_ref FROM eacp.connectors`)
	if err != nil {
		return st, err
	}
	var c ConnectorState
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&c.ID, &name, &c.Protocol, &c.Endpoint, &c.SecretRef}, func() error {
		c.Tools = map[string]ToolState{}
		st.Connectors[name] = c
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `
		SELECT t.id, c.name, t.name, t.definition_id, t.quarantined_at IS NOT NULL, t.active_contract_id,
		       ct.id IS NOT NULL AND ct.revoked_at IS NULL,
		       COALESCE(ct.side_effects, '{}'), COALESCE(ct.idempotency_mode, ''),
		       COALESCE(ct.idempotency_key_field, ''), COALESCE(ct.correlation_field, ''),
		       COALESCE(ct.reconciliation_lookup, ''), COALESCE(ct.reconciliation_consistency, ''),
		       COALESCE(ct.proof_standard, ''), COALESCE(ct.no_effect_errors, '{}'), COALESCE(ct.max_attempts, 0),
		       COALESCE(ct.timeout_ms, 0), COALESCE(ct.concurrency_group, ''), COALESCE(ct.max_inflight, 0),
		       COALESCE(ct.schedule_priority, 0), COALESCE(ct.data_sensitivity, ''), COALESCE(ct.cost_unit, ''),
		       COALESCE(ct.cost_fixed::text, '0'), COALESCE(ct.cost_amount_field, ''),
		       COALESCE(ct.cost_unit_field, ''), COALESCE(ct.max_queued, 0),
		       COALESCE(ct.retry_max_elapsed_ms, 0), COALESCE(ct.retry_max_cost::text, ''), ct.definition_id
		FROM eacp.tools t
		JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		LEFT JOIN eacp.tool_contracts ct ON ct.tenant_id = t.tenant_id AND ct.id = t.active_contract_id`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var ts ToolState
		var conn, tool, costFixed, retryCost string
		var live bool
		var ct registry.Contract
		if err := rows.Scan(&ts.ID, &conn, &tool, &ts.DefinitionID, &ts.Quarantined, &ts.ActiveContractID, &live,
			&ct.SideEffects, &ct.IdempotencyMode, &ct.IdempotencyKeyField, &ct.CorrelationField,
			&ct.ReconciliationLookup, &ct.ReconciliationConsistency, &ct.ProofStandard, &ct.NoEffectErrors,
			&ct.MaxAttempts, &ct.TimeoutMS, &ct.ConcurrencyGroup, &ct.MaxInflight, &ct.SchedulePriority,
			&ct.DataSensitivity, &ct.CostUnit, &costFixed, &ct.CostAmountField, &ct.CostUnitField, &ct.MaxQueued,
			&ct.RetryMaxElapsedMS, &retryCost, &ct.DefinitionID); err != nil {
			rows.Close()
			return st, err
		}
		if live {
			ct.CostFixed, ct.RetryMaxCost = json.Number(costFixed), json.Number(retryCost)
			ts.ActiveContract = &ct
		}
		st.Connectors[conn].Tools[tool] = ts
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT id, name, display_name, environment, risk_class,
		COALESCE(owner_principal_id, '00000000-0000-0000-0000-000000000000'),
		COALESCE(owner_group_id, '00000000-0000-0000-0000-000000000000') FROM eacp.agents`)
	if err != nil {
		return st, err
	}
	var ag AgentState
	byID := map[uuid.UUID]string{}
	if _, err := pgx.ForEachRow(rows, []any{&ag.ID, &name, &ag.DisplayName, &ag.Environment, &ag.RiskClass,
		&ag.OwnerPrincipalID, &ag.OwnerGroupID}, func() error {
		st.Agents[name] = ag
		byID[ag.ID] = name
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `
		SELECT v.agent_id, v.id, v.version, v.runtime, v.code_ref, v.state,
		       COALESCE((SELECT array_agg(c.name || '.' || t.name) FROM eacp.agent_allowlists al
		                 JOIN eacp.tools t ON t.tenant_id = al.tenant_id AND t.id = ANY (al.tool_ids)
		                 JOIN eacp.connectors c ON c.tenant_id = t.tenant_id AND c.id = t.connector_id
		                 WHERE al.tenant_id = v.tenant_id AND al.id = v.active_allowlist_id), '{}')
		FROM eacp.agent_versions v ORDER BY v.agent_id, v.version DESC`)
	if err != nil {
		return st, err
	}
	var agentID uuid.UUID
	var v VersionState
	if _, err := pgx.ForEachRow(rows, []any{&agentID, &v.ID, &v.Number, &v.Runtime, &v.CodeRef, &v.State,
		&v.AllowedTools}, func() error {
		n := byID[agentID]
		a := st.Agents[n]
		x := v
		x.AllowedTools = slices.Clone(v.AllowedTools)
		slices.Sort(x.AllowedTools)
		a.Versions = append(a.Versions, x)
		st.Agents[n] = a
		return nil
	}); err != nil {
		return st, err
	}

	rows, err = tx.Query(ctx, `SELECT name, id FROM eacp.principals WHERE disabled_at IS NULL`)
	if err != nil {
		return st, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&name, &id}, func() error { st.Principals[name] = id; return nil }); err != nil {
		return st, err
	}
	rows, err = tx.Query(ctx, `SELECT name, id FROM eacp.groups`)
	if err != nil {
		return st, err
	}
	_, err = pgx.ForEachRow(rows, []any{&name, &id}, func() error { st.Groups[name] = id; return nil })
	return st, err
}
```

- [ ] **Step 4: Implement `plan.go`**

`internal/bundle/plan.go`:

```go
package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"

	"eacp/internal/registry"
)

// Step operations and stages.
const (
	OpCreate     = "create"
	OpPropose    = "propose"
	OpActivate   = "activate"
	OpTransition = "transition"
	OpRevoke     = "revoke"
	OpImport     = "import"

	StageSubmit  = "submit"
	StageApprove = "approve"
)

// Step is one registry write of a change set.
type Step struct {
	Ordinal  int        `json:"ordinal"`
	Address  string     `json:"address"`
	Op       string     `json:"op"`
	Stage    string     `json:"stage"`
	Payload  Payload    `json:"payload"`
	ObjectID *uuid.UUID `json:"object_id,omitempty"`
}

// Payload is what a step needs. Parent names the object the step acts on
// or under: ParentID when it exists at plan time, else the address of the
// step of this change set that creates or imports it.
type Payload struct {
	Connector    *registry.NewConnector `json:"connector,omitempty"`
	Agent        *registry.NewAgent     `json:"agent,omitempty"`
	Version      *registry.NewVersion   `json:"version,omitempty"`
	Contract     *registry.Contract     `json:"contract,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Tools        []string               `json:"tools,omitempty"`
	Parent       string                 `json:"parent,omitempty"`
	ParentID     *uuid.UUID             `json:"parent_id,omitempty"`
	ProposalStep int                    `json:"proposal_step,omitempty"`
	To           registry.State         `json:"to,omitempty"`
	Reason       string                 `json:"reason,omitempty"`
	ObjectID     *uuid.UUID             `json:"object_id,omitempty"`
}

// Ref is a registry object a plan read; the change set's digest covers it.
type Ref struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

// Diff is a plan before it is recorded.
type Diff struct {
	Steps    []Step
	Findings []Finding
	Refs     []Ref
}

type planner struct {
	doc      Document
	st       State
	prune    bool
	reason   string
	managed  map[string]uuid.UUID
	submit   []Step
	approve  []Step
	findings []Finding
	refs     map[Ref]bool
	declared map[string]bool // "connector.tool" of the tools this plan keeps or creates
}

// diff computes the steps that converge st to doc. It is pure: the same
// document and state always give the same plan.
func diff(bundle string, changeSet uuid.UUID, doc Document, st State, prune bool) Diff {
	p := &planner{doc: doc, st: st, prune: prune,
		reason:  fmt.Sprintf("bundle %s change set %s", bundle, changeSet),
		managed: map[string]uuid.UUID{}, refs: map[Ref]bool{}, declared: map[string]bool{}}
	for k, v := range st.Managed {
		p.managed[k] = v
	}
	p.imports()
	for _, name := range sortedKeys(doc.Connectors) {
		p.connector(name)
	}
	for _, name := range sortedKeys(doc.Agents) {
		p.agent(name)
	}
	p.orphans()

	steps := append(p.submit, p.approve...)
	for i := range steps {
		steps[i].Ordinal = i + 1
	}
	if len(steps) > MaxSteps {
		p.find("bundle", KindInvalid, "the plan has %d steps; a change set has at most %d: split the bundle",
			len(steps), MaxSteps)
	}
	refs := make([]Ref, 0, len(p.refs))
	for r := range p.refs {
		refs = append(refs, r)
	}
	slices.SortFunc(refs, func(a, b Ref) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	return Diff{Steps: steps, Findings: p.findings, Refs: refs}
}

func (p *planner) find(addr, kind, format string, args ...any) {
	p.findings = append(p.findings, Finding{Address: addr, Kind: kind, Detail: fmt.Sprintf(format, args...)})
}

func (p *planner) ref(kind string, id uuid.UUID) { p.refs[Ref{kind, id}] = true }

// add appends a step and returns its ordinal (only submit ordinals are
// final while planning; the approve stage follows the whole submit stage).
func (p *planner) add(stage, addr, op string, pl Payload) int {
	s := Step{Address: addr, Op: op, Stage: stage, Payload: pl}
	if stage == StageSubmit {
		p.submit = append(p.submit, s)
		return len(p.submit)
	}
	p.approve = append(p.approve, s)
	return 0
}

// parent names addr's object: by id when it exists, else by address.
func parent(addr string, id uuid.UUID) (string, *uuid.UUID) {
	if id != uuid.Nil {
		return "", &id
	}
	return addr, nil
}

// owned reports whether this bundle manages the existing object at addr.
func (p *planner) owned(addr string, id uuid.UUID) bool {
	if p.managed[addr] == id {
		return true
	}
	if other := p.st.ManagedElsewhere[id]; other != "" {
		p.find(addr, KindUnmanaged, "managed by bundle %s", other)
		return false
	}
	p.find(addr, KindUnmanaged, "exists (%s) and is not managed by this bundle: add an import", id)
	return false
}

func (p *planner) imports() {
	for _, im := range p.doc.Imports {
		kind, name, _ := strings.Cut(im.To, ".")
		if !p.st.names(kind, name, im.ID) {
			p.find(im.To, KindUnresolvedReference, "import id %s is not the %s named %s", im.ID, kind, name)
			continue
		}
		if p.managed[im.To] == im.ID {
			continue
		}
		if other := p.st.ManagedElsewhere[im.ID]; other != "" {
			p.find(im.To, KindUnmanaged, "managed by bundle %s", other)
			continue
		}
		id := im.ID
		p.add(StageSubmit, im.To, OpImport, Payload{ObjectID: &id})
		p.managed[im.To] = im.ID
		p.ref(kind, im.ID)
	}
}

// names reports whether id is the object that kind and name denote.
func (st State) names(kind, name string, id uuid.UUID) bool {
	switch kind {
	case "connector":
		return st.Connectors[name].ID == id && id != uuid.Nil
	case "tool":
		conn, tool, _ := strings.Cut(name, ".")
		t, ok := st.Connectors[conn].Tools[tool]
		return ok && t.ID == id
	case "agent":
		return st.Agents[name].ID == id && id != uuid.Nil
	case "version":
		return slices.ContainsFunc(st.Agents[name].Versions, func(v VersionState) bool { return v.ID == id })
	}
	return false
}

func (p *planner) connector(name string) {
	want, addr := p.doc.Connectors[name], "connector."+name
	cur, exists := p.st.Connectors[name]
	var id uuid.UUID
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Connector: &registry.NewConnector{Name: name,
			Protocol: want.Protocol, Endpoint: want.Endpoint, SecretRef: want.SecretRef}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("connector", id)
		for field, pair := range map[string][2]string{"protocol": {cur.Protocol, want.Protocol},
			"endpoint": {cur.Endpoint, want.Endpoint}, "secret_ref": {cur.SecretRef, want.SecretRef}} {
			if pair[0] != pair[1] {
				p.find(addr, KindUnsupported, "%s is immutable: %q in the registry, %q in the bundle", field,
					pair[0], pair[1])
			}
		}
	}
	for _, tn := range sortedKeys(want.Tools) {
		p.tool(name, tn, want, cur.Tools, id)
	}
}

func (p *planner) tool(conn, name string, want Connector, tools map[string]ToolState, connID uuid.UUID) {
	addr, caddr := "tool."+conn+"."+name, "contract."+conn+"."+name
	ts, exists := tools[name]
	var toolID uuid.UUID
	switch {
	case !exists && want.Protocol == "mcp":
		p.find(addr, KindUnresolvedReference, "MCP tools are discovered, never declared (ADR-023): scan the connector first")
		return
	case !exists:
		par, parID := parent("connector."+conn, connID)
		p.add(StageSubmit, addr, OpCreate, Payload{Name: name, Parent: par, ParentID: parID})
	default:
		toolID = ts.ID
		p.ref("tool", ts.ID)
		if ts.ActiveContractID != nil {
			p.ref("contract", *ts.ActiveContractID)
		}
	}
	p.declared[conn+"."+name] = true
	c := want.Tools[name].Contract
	if c == nil {
		return
	}
	desired := *c
	if want.Protocol == "mcp" {
		if ts.Quarantined {
			p.find(caddr, KindContained, "the tool is quarantined: release it through the registry (ADR-023)")
			return
		}
		if ts.DefinitionID == nil {
			p.find(caddr, KindUnresolvedReference, "the tool has no current definition")
			return
		}
		def := *ts.DefinitionID
		desired.DefinitionID = &def
	}
	if exists && ts.ActiveContract != nil && sameContract(*ts.ActiveContract, desired) {
		return
	}
	par, parID := parent(addr, toolID)
	n := p.add(StageSubmit, caddr, OpPropose, Payload{Contract: &desired, Parent: par, ParentID: parID})
	p.add(StageApprove, caddr, OpActivate, Payload{Parent: par, ParentID: parID, ProposalStep: n})
}

func (p *planner) agent(name string) {
	want, addr := p.doc.Agents[name], "agent."+name
	cur, exists := p.st.Agents[name]
	var id uuid.UUID
	na := registry.NewAgent{Name: name, DisplayName: want.DisplayName, Environment: want.Environment,
		RiskClass: want.RiskClass}
	if !p.owner(addr, want.Owner, &na) {
		return
	}
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Agent: &na})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("agent", id)
		for field, pair := range map[string][2]string{"display_name": {cur.DisplayName, want.DisplayName},
			"environment": {cur.Environment, want.Environment}, "risk_class": {cur.RiskClass, want.RiskClass},
			"owner": {cur.OwnerPrincipalID.String() + cur.OwnerGroupID.String(),
				na.OwnerPrincipalID.String() + na.OwnerGroupID.String()}} {
			if pair[0] != pair[1] {
				p.find(addr, KindUnsupported, "%s is immutable: agents are never changed in place", field)
			}
		}
	}
	p.version(name, want, cur, id)
}

func (p *planner) owner(addr string, o Owner, na *registry.NewAgent) bool {
	if o.Principal != "" {
		id, ok := p.st.Principals[o.Principal]
		if !ok {
			p.find(addr, KindUnresolvedReference, "no enabled principal %q", o.Principal)
			return false
		}
		na.OwnerPrincipalID = id
		p.ref("principal", id)
		return true
	}
	id, ok := p.st.Groups[o.Group]
	if !ok {
		p.find(addr, KindUnresolvedReference, "no group %q", o.Group)
		return false
	}
	na.OwnerGroupID = id
	p.ref("group", id)
	return true
}

func (p *planner) version(name string, want Agent, cur AgentState, agentID uuid.UUID) {
	vaddr, laddr := "version."+name, "allowlist."+name
	var v *VersionState
	for i := range cur.Versions { // newest first
		x := &cur.Versions[i]
		p.ref("version", x.ID)
		if v == nil && x.Runtime == want.Version.Runtime && x.CodeRef == want.Version.CodeRef &&
			x.State != registry.StateRetired && x.State != registry.StateRevoked {
			v = x
		}
	}
	old := p.managed[vaddr]
	var versionID uuid.UUID
	switch {
	case v != nil && v.ID == old:
		versionID = v.ID
	case v != nil:
		versionID = v.ID
		id := v.ID
		p.add(StageSubmit, vaddr, OpImport, Payload{ObjectID: &id})
	default:
		par, parID := parent("agent."+name, agentID)
		p.add(StageSubmit, vaddr, OpCreate, Payload{Version: &registry.NewVersion{Runtime: want.Version.Runtime,
			CodeRef: want.Version.CodeRef}, Parent: par, ParentID: parID})
	}
	if old != uuid.Nil && old != versionID {
		p.replaced(vaddr, cur, old)
	}

	tools := slices.Clone(want.Allowlist)
	slices.Sort(tools)
	for _, t := range tools {
		p.toolRef(laddr, t)
	}
	var current []string
	if v != nil {
		current = v.AllowedTools
	}
	if len(tools) > 0 && !slices.Equal(tools, current) {
		par, parID := parent(vaddr, versionID)
		n := p.add(StageSubmit, laddr, OpPropose, Payload{Tools: tools, Parent: par, ParentID: parID})
		p.add(StageApprove, laddr, OpActivate, Payload{Parent: par, ParentID: parID, ProposalStep: n})
	}

	if want.State != string(registry.StateActive) {
		return
	}
	if v != nil {
		switch v.State {
		case registry.StateActive:
			return
		case registry.StateSuspended, registry.StateQuarantined:
			p.find(vaddr, KindContained, "version %d is %s: resume or release it through fleet operations (ADR-024)",
				v.Number, v.State)
			return
		}
	}
	for _, x := range cur.Versions {
		if x.State == registry.StateActive && (v == nil || x.ID != v.ID) {
			p.find(vaddr, KindRequiresRelease, "version %d is ACTIVE: promote the new version with a release (ADR-018)",
				x.Number)
			return
		}
	}
	par, parID := parent(vaddr, versionID)
	p.add(StageApprove, vaddr, OpTransition, Payload{To: registry.StateActive, Reason: p.reason, Parent: par,
		ParentID: parID})
}

// replaced handles a version the bundle managed but no longer wants: with
// prune a REGISTERED or SUSPENDED one is retired; an ACTIVE one never is.
func (p *planner) replaced(addr string, cur AgentState, old uuid.UUID) {
	for _, x := range cur.Versions {
		if x.ID != old {
			continue
		}
		switch {
		case x.State == registry.StateRetired || x.State == registry.StateRevoked:
		case p.prune && (x.State == registry.StateRegistered || x.State == registry.StateSuspended):
			id := x.ID
			p.add(StageApprove, addr, OpTransition, Payload{To: registry.StateRetired, Reason: p.reason + " prune",
				ParentID: &id})
		default:
			p.find(addr, KindOrphan, "version %d (%s) is no longer declared; nothing is retired while it is %s",
				x.Number, x.State, x.State)
		}
	}
}

func (p *planner) toolRef(addr, ref string) {
	if p.declared[ref] {
		return
	}
	conn, tool, _ := strings.Cut(ref, ".")
	if ts, ok := p.st.Connectors[conn].Tools[tool]; ok {
		p.ref("tool", ts.ID)
		p.find("tool."+ref, KindUnmanagedReference, "%s uses %s, which this bundle does not declare", addr, ref)
		return
	}
	p.find(addr, KindUnresolvedReference, "no tool %s", ref)
}

func (p *planner) orphans() {
	for _, addr := range sortedKeys(p.st.Managed) {
		if declared(p.doc, addr) {
			continue
		}
		kind, name, _ := strings.Cut(addr, ".")
		switch kind {
		case "tool":
			conn, tool, _ := strings.Cut(name, ".")
			ts := p.st.Connectors[conn].Tools[tool]
			if p.prune && ts.ActiveContract != nil {
				id := *ts.ActiveContractID
				p.ref("contract", id)
				p.add(StageApprove, "contract."+name, OpRevoke, Payload{ObjectID: &id, Reason: p.reason + " prune"})
				continue
			}
		case "version":
			p.replaced(addr, p.st.Agents[name], p.st.Managed[addr])
			continue
		}
		p.find(addr, KindOrphan, "%s is managed by this bundle but no longer declared; nothing is deleted", addr)
	}
}

// sameContract compares contracts as the database stores them: arrays as
// sets and numbers by value.
func sameContract(a, b registry.Contract) bool {
	ja, _ := json.Marshal(normalContract(a))
	jb, _ := json.Marshal(normalContract(b))
	return bytes.Equal(ja, jb)
}

func normalContract(c registry.Contract) registry.Contract {
	c.SideEffects = sortedCopy(c.SideEffects)
	c.NoEffectErrors = sortedCopy(c.NoEffectErrors)
	c.CostFixed = normalNumber(c.CostFixed, "0")
	c.RetryMaxCost = normalNumber(c.RetryMaxCost, "")
	return c
}

func normalNumber(n json.Number, empty string) json.Number {
	if n == "" {
		n = json.Number(empty)
	}
	if n == "" {
		return ""
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		return n
	}
	return json.Number(r.RatString())
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}
```

Note: the allowlist "orphan" case of an agent dropped from the document is handled by `orphans()` through `agent.X` (orphan) and `version.X` (`replaced`, which never retires an ACTIVE version). That is what `TestReferencesAndOrphans` expects.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Diff|Contract|Release|Adopt|Containment|Import|MCP|References|Bounded'`
Expected: PASS. If `TestPlansAreBoundedTo2000Steps` reports fewer than 2000 steps, raise the loop count so that 3 steps × N > 2000 (700 gives 2800).

- [ ] **Step 6: Commit**

```bash
git add internal/bundle/state.go internal/bundle/plan.go internal/bundle/plan_test.go
git commit -m "feat(bundle): registry state and a pure, ordered diff"
```

---

### Task 5: Recording plans: `Service.Plan`, `Get`, `List`, `Bundles`

**Files:**
- Create: `internal/bundle/service.go`
- Test: `internal/bundle/service_test.go`

**Interfaces:**
- Consumes: `Decode`, `Validate`, `blocked`, `slugRE` (Task 3); `loadState`, `diff`, `Step`, `Payload` (Task 4); `storage.InTenantSnapshotTx` (Task 2); `registry.Actor`, `registry.Error`, `registry.MapErr`.
- Produces: `New(pool *pgxpool.Pool) *Service`; `type Request{Bundle string; Desired json.RawMessage; Prune, DryRun bool}`; `type ChangeSet` (fields below); `(*Service).Plan(ctx, registry.Actor, Request) (ChangeSet, error)`; `Get(ctx, a, id uuid.UUID) (ChangeSet, error)`; `List(ctx, a, bundle string, limit int) ([]ChangeSet, error)`; `Bundles(ctx, a) ([]BundleInfo, error)`; `type Error{Code, Msg, Address string; Findings []Finding; Err error}`; codes `CodePlanBlocked`, `CodeStale`, `CodeOpen`, `CodeSamePrincipal`, `CodeStepFailed`; `(*Service).change`; `classify(error) error`.

- [ ] **Step 1: Write the failing integration tests**

`internal/bundle/service_test.go`:

```go
package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/bundle"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

const ledgerDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func setup(t *testing.T) (*registrytest.Fixture, *bundle.Service) {
	t.Helper()
	f := registrytest.New(t)
	return f, bundle.New(f.App)
}

func as(f *registrytest.Fixture, name string) registry.Actor {
	return registry.Actor{TenantID: f.Tenant, PrincipalID: f.P[name]}
}

func req(doc string) bundle.Request {
	return bundle.Request{Bundle: "ledger", Desired: json.RawMessage(doc)}
}

func count(t *testing.T, f *registrytest.Fixture, sql string, args ...any) int {
	t.Helper()
	var n int
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), sql, args...).Scan(&n)
	}))
	return n
}

func wantCode(t *testing.T, err error, code string) *bundle.Error {
	t.Helper()
	var be *bundle.Error
	if !errors.As(err, &be) || be.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	return be
}

func TestPlanRecordsAChangeSetAndWritesNoRegistryObject(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()

	dry := req(ledgerDoc)
	dry.DryRun = true
	p, err := s.Plan(ctx, as(f, "erin"), dry)
	ok(t, err)
	if p.ID != uuid.Nil || len(p.Steps) != 9 || count(t, f, `SELECT count(*) FROM eacp.change_sets`) != 0 {
		t.Fatalf("dry run = %+v", p)
	}

	p, err = s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if p.ID == uuid.Nil || p.State != "PLANNED" || len(p.Steps) != 9 || len(p.BaseDigest) != 64 ||
		len(p.DesiredDigest) != 64 || p.PlannedBy != f.P["erin"] {
		t.Fatalf("plan = %+v", p)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`) +
		count(t, f, `SELECT count(*) FROM eacp.agents WHERE name = 'ledger-bot'`); n != 0 {
		t.Fatalf("a plan wrote %d registry objects", n)
	}
	got, err := s.Get(ctx, as(f, "audra"), p.ID)
	ok(t, err)
	if got.Bundle != "ledger" || len(got.Steps) != 9 || got.Steps[1].Payload.Parent != "connector.ledger" {
		t.Fatalf("get = %+v", got)
	}
	list, err := s.List(ctx, as(f, "audra"), "ledger", 10)
	ok(t, err)
	if len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("list = %+v", list)
	}
	bundles, err := s.Bundles(ctx, as(f, "audra"))
	ok(t, err)
	if len(bundles) != 1 || bundles[0].Name != "ledger" {
		t.Fatalf("bundles = %+v", bundles)
	}
}

func TestBlockedPlansAreNotRecorded(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	_, err := s.Plan(ctx, as(f, "erin"), req(strings.Replace(ledgerDoc, "carol", "nobody", 1)))
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if len(be.Findings) == 0 || be.Findings[0].Kind != bundle.KindUnresolvedReference {
		t.Fatalf("findings = %+v", be.Findings)
	}
	_, err = s.Plan(ctx, as(f, "erin"), req(strings.Replace(ledgerDoc, "git:aaa111", "${var.ref}", 1)))
	wantCode(t, err, bundle.CodePlanBlocked)
	_, err = s.Plan(ctx, as(f, "erin"), bundle.Request{Bundle: "Bad Name", Desired: json.RawMessage(ledgerDoc)})
	var re *registry.Error
	if !errors.As(err, &re) || !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("bad bundle name: %v", err)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.change_sets`); n != 0 {
		t.Fatalf("%d change sets recorded", n)
	}
}

func TestReplanningSupersedesAPlannedChangeSet(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	first, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	second, err := s.Plan(ctx, as(f, "rita"), req(ledgerDoc))
	ok(t, err)
	got, err := s.Get(ctx, as(f, "erin"), first.ID)
	ok(t, err)
	if got.State != "SUPERSEDED" || !strings.Contains(got.CloseReason, second.ID.String()) {
		t.Fatalf("first = %+v", got)
	}
	_, err = s.Plan(ctx, as(f, "carol"), req(ledgerDoc))
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("carol planned: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/bundle/ -run 'PlanRecords|Blocked|Replanning'`
Expected: FAIL to compile: `undefined: bundle.New`.

- [ ] **Step 3: Implement `service.go`**

`internal/bundle/service.go`:

```go
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"eacp/internal/registry"
	"eacp/internal/storage"
)

// Error codes the API reports for change-set failures.
const (
	CodePlanBlocked   = "plan_blocked"
	CodeStale         = "change_set_stale"
	CodeOpen          = "change_set_open"
	CodeSamePrincipal = "same_principal"
	CodeStepFailed    = "step_failed"
)

// Error is a change-set failure with a stable code. Err, when set, is the
// registry error a failing step returned.
type Error struct {
	Code     string    `json:"error"`
	Msg      string    `json:"detail"`
	Address  string    `json:"address,omitempty"`
	Findings []Finding `json:"findings,omitempty"`
	Err      error     `json:"-"`
}

func (e *Error) Error() string { return "bundle: " + e.Code + ": " + e.Msg }
func (e *Error) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrInvalid, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &registry.Error{Kind: registry.ErrConflict, Msg: fmt.Sprintf(format, args...)}
}

// classify maps database errors: a serialization failure or a stale digest
// (40001) means the registry moved on, and the bundle must be planned again.
func classify(err error) error {
	var be *Error
	var re *registry.Error
	if err == nil || errors.As(err, &be) || errors.As(err, &re) {
		return err
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && (p.Code == "40001" || p.Code == "40P01") {
		return &Error{Code: CodeStale, Msg: p.Message + ": plan the bundle again"}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such change set"}
	}
	return registry.MapErr(err)
}

// Service plans and applies change sets over a pool connected as the
// application role.
type Service struct{ pool *pgxpool.Pool }

// New returns a Service.
func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Request asks for a plan of a bundle's desired state.
type Request struct {
	Bundle  string          `json:"bundle"`
	Desired json.RawMessage `json:"desired"`
	Prune   bool            `json:"prune,omitempty"`
	DryRun  bool            `json:"dry_run,omitempty"`
}

// ChangeSet is a recorded change set or, without an ID, a plan that was not
// recorded (a dry run, or nothing to change).
type ChangeSet struct {
	ID            uuid.UUID  `json:"id,omitzero"`
	Bundle        string     `json:"bundle"`
	State         string     `json:"state,omitempty"`
	Prune         bool       `json:"prune"`
	DryRun        bool       `json:"dry_run,omitempty"`
	DesiredDigest string     `json:"desired_digest,omitempty"`
	BaseDigest    string     `json:"base_digest,omitempty"`
	SealedDigest  string     `json:"sealed_digest,omitempty"`
	PlannedBy     uuid.UUID  `json:"planned_by,omitzero"`
	PlannedAt     *time.Time `json:"planned_at,omitempty"`
	SubmittedBy   *uuid.UUID `json:"submitted_by,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	ApprovedBy    *uuid.UUID `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
	ClosedBy      *uuid.UUID `json:"closed_by,omitempty"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	CloseReason   string     `json:"close_reason,omitempty"`
	Steps         []Step     `json:"steps"`
	Findings      []Finding  `json:"findings,omitempty"`
}

// BundleInfo is a bundle, how many objects it manages and its latest
// applied change set.
type BundleInfo struct {
	Name        string     `json:"name"`
	Managed     int        `json:"managed"`
	LastApplied *uuid.UUID `json:"last_applied,omitempty"`
}

func (s *Service) change(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	return classify(storage.InTenantTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		return fn(tx)
	}))
}

func (s *Service) read(ctx context.Context, a registry.Actor, fn func(pgx.Tx) error) error {
	if a.TenantID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no tenant"}
	}
	return classify(storage.InTenantReadTx(ctx, s.pool, a.TenantID.String(), fn))
}

// Plan diffs the desired document against the registry in one snapshot. It
// records a change set unless the request is a dry run, the plan has a
// blocking finding, or there is nothing to change. It never writes a
// registry object.
func (s *Service) Plan(ctx context.Context, a registry.Actor, req Request) (ChangeSet, error) {
	if a.TenantID == uuid.Nil || a.PrincipalID == uuid.Nil {
		return ChangeSet{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	if !slugRE.MatchString(req.Bundle) {
		return ChangeSet{}, invalid("bundle names match %s", slugRE)
	}
	doc, err := Decode(req.Desired)
	if err != nil {
		return ChangeSet{}, invalid("%v", err)
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return ChangeSet{}, err
	}
	out := ChangeSet{Bundle: req.Bundle, Prune: req.Prune, DryRun: req.DryRun, Steps: []Step{},
		Findings: Validate(doc, req.Desired)}
	if blocked(out.Findings) {
		return out, &Error{Code: CodePlanBlocked, Msg: "the bundle is invalid", Findings: out.Findings}
	}
	id := uuid.New()
	recorded := false
	err = storage.InTenantSnapshotTx(ctx, s.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, a.PrincipalID); err != nil {
			return err
		}
		st, err := loadState(ctx, tx, req.Bundle)
		if err != nil {
			return err
		}
		d := diff(req.Bundle, id, doc, st, req.Prune)
		out.Steps, out.Findings = d.Steps, append(out.Findings, d.Findings...)
		if blocked(out.Findings) {
			return &Error{Code: CodePlanBlocked, Msg: "the plan has blocking findings", Findings: out.Findings}
		}
		if req.DryRun || len(d.Steps) == 0 {
			return nil
		}
		recorded = true
		return record(ctx, tx, id, req, canonical, d)
	})
	if err != nil {
		return out, classify(err)
	}
	if !recorded {
		return out, nil
	}
	cs, err := s.Get(ctx, a, id)
	cs.Findings = out.Findings
	return cs, err
}

// record writes the change set, its steps and refs, and seals it. An open
// PLANNED change set of the bundle is superseded; a SUBMITTED one blocks.
func record(ctx context.Context, tx pgx.Tx, id uuid.UUID, req Request, desired []byte, d Diff) error {
	var open uuid.UUID
	var state string
	err := tx.QueryRow(ctx, `SELECT cs.id, cs.state FROM eacp.change_sets cs
		JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id
		WHERE b.name = $1 AND cs.state IN ('PLANNED', 'SUBMITTED') FOR UPDATE OF cs`, req.Bundle).Scan(&open, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case state == "SUBMITTED":
		return &Error{Code: CodeOpen, Msg: fmt.Sprintf(
			"change set %s of bundle %s awaits approval: approve or reject it first", open, req.Bundle)}
	default:
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = 'SUPERSEDED', close_reason = $2 WHERE id = $1`,
			open, "superseded by change set "+id.String()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.bundles (tenant_id, name) VALUES (eacp.current_tenant_id(), $1)
		ON CONFLICT (tenant_id, name) DO NOTHING`, req.Bundle); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_sets (tenant_id, id, bundle_id, state, desired, prune)
		SELECT eacp.current_tenant_id(), $1, id, 'PLANNED', $3::jsonb, $4 FROM eacp.bundles WHERE name = $2`,
		id, req.Bundle, string(desired), req.Prune); err != nil {
		return err
	}
	for _, st := range d.Steps {
		payload, err := json.Marshal(st.Payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_steps
			(tenant_id, change_set_id, ordinal, address, op, stage, payload)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6::jsonb)`,
			id, st.Ordinal, st.Address, st.Op, st.Stage, string(payload)); err != nil {
			return err
		}
	}
	for _, r := range d.Refs {
		if _, err := tx.Exec(ctx, `INSERT INTO eacp.change_set_refs (tenant_id, change_set_id, kind, object_id)
			VALUES (eacp.current_tenant_id(), $1, $2, $3)`, id, r.Kind, r.ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
	return err
}

const changeSetColumns = `cs.id, b.name, cs.state, cs.prune, encode(cs.desired_digest, 'hex'),
	COALESCE(encode(cs.base_digest, 'hex'), ''), COALESCE(encode(cs.sealed_digest, 'hex'), ''),
	cs.planned_by, cs.planned_at, cs.submitted_by, cs.submitted_at, cs.approved_by, cs.approved_at,
	cs.closed_by, cs.closed_at, COALESCE(cs.close_reason, '')
	FROM eacp.change_sets cs JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id`

func scanChangeSet(r pgx.Row) (ChangeSet, error) {
	c := ChangeSet{Steps: []Step{}}
	var plannedAt time.Time
	err := r.Scan(&c.ID, &c.Bundle, &c.State, &c.Prune, &c.DesiredDigest, &c.BaseDigest, &c.SealedDigest,
		&c.PlannedBy, &plannedAt, &c.SubmittedBy, &c.SubmittedAt, &c.ApprovedBy, &c.ApprovedAt,
		&c.ClosedBy, &c.ClosedAt, &c.CloseReason)
	c.PlannedAt = &plannedAt
	return c, err
}

func loadSteps(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]Step, error) {
	rows, err := tx.Query(ctx, `SELECT ordinal, address, op, stage, payload, object_id
		FROM eacp.change_set_steps WHERE change_set_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Step, error) {
		var s Step
		var payload []byte
		if err := r.Scan(&s.Ordinal, &s.Address, &s.Op, &s.Stage, &payload, &s.ObjectID); err != nil {
			return s, err
		}
		return s, json.Unmarshal(payload, &s.Payload)
	})
}

// Get returns a change set with its steps.
func (s *Service) Get(ctx context.Context, a registry.Actor, id uuid.UUID) (ChangeSet, error) {
	var c ChangeSet
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var err error
		if c, err = scanChangeSet(tx.QueryRow(ctx, `SELECT `+changeSetColumns+` WHERE cs.id = $1`, id)); err != nil {
			return err
		}
		c.Steps, err = loadSteps(ctx, tx, id)
		return err
	})
	return c, err
}

// List returns change sets, newest first, optionally of one bundle, without
// their steps.
func (s *Service) List(ctx context.Context, a registry.Actor, bundle string, limit int) ([]ChangeSet, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []ChangeSet{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+changeSetColumns+`
			WHERE $1 = '' OR b.name = $1 ORDER BY cs.planned_at DESC LIMIT $2`, bundle, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChangeSet, error) { return scanChangeSet(r) })
		return err
	})
	return out, err
}

// Bundles lists the tenant's bundles.
func (s *Service) Bundles(ctx context.Context, a registry.Actor) ([]BundleInfo, error) {
	out := []BundleInfo{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT b.name,
			(SELECT count(*) FROM eacp.bundle_resources br WHERE br.tenant_id = b.tenant_id AND br.bundle_id = b.id),
			(SELECT cs.id FROM eacp.change_sets cs WHERE cs.tenant_id = b.tenant_id AND cs.bundle_id = b.id
			   AND cs.state = 'APPLIED' ORDER BY COALESCE(cs.approved_at, cs.submitted_at) DESC LIMIT 1)
			FROM eacp.bundles b ORDER BY b.name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (BundleInfo, error) {
			var b BundleInfo
			return b, r.Scan(&b.Name, &b.Managed, &b.LastApplied)
		})
		return err
	})
	return out, err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/bundle/ -run 'PlanRecords|Blocked|Replanning'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bundle/service.go internal/bundle/service_test.go
git commit -m "feat(bundle): record, supersede and read change sets"
```

---

### Task 6: Submit, approve and reject

**Files:**
- Create: `internal/bundle/execute.go`
- Modify: `internal/bundle/service_test.go` (add tests)

**Interfaces:**
- Consumes: `Service.change`, `classify`, `loadSteps`, `Error`, codes (Task 5); `registry.Tx` (Task 2); `Step`, `Payload`, `Op*`, `Stage*` (Task 4).
- Produces: `(*Service).Submit(ctx, a, id uuid.UUID) (ChangeSet, error)`, `Approve(ctx, a, id) (ChangeSet, error)`, `Reject(ctx, a, id, reason string) (ChangeSet, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/bundle/service_test.go`:

```go
// apply plans ledgerDoc as erin, submits it as erin and approves it as rita.
func apply(t *testing.T, f *registrytest.Fixture, s *bundle.Service, doc string) bundle.ChangeSet {
	t.Helper()
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(doc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	cs, err := s.Approve(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	return cs
}

func TestSubmitAndApproveApplyTheBundle(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	sub, err := s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	if sub.State != "SUBMITTED" || len(sub.SealedDigest) != 64 || *sub.SubmittedBy != f.P["erin"] {
		t.Fatalf("submitted = %+v", sub)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions v JOIN eacp.agents a ON a.id = v.agent_id
		WHERE a.name = 'ledger-bot' AND v.state = 'REGISTERED' AND v.active_allowlist_id IS NULL`); n != 1 {
		t.Fatalf("after submit: %d registered versions without an allowlist", n)
	}
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("second submit: %v", err)
	}

	cs, err := s.Approve(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	if cs.State != "APPLIED" || *cs.ApprovedBy != f.P["rita"] {
		t.Fatalf("applied = %+v", cs)
	}
	for _, st := range cs.Steps {
		if st.ObjectID == nil {
			t.Fatalf("step %d did not run", st.Ordinal)
		}
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.agent_versions v JOIN eacp.agents a ON a.id = v.agent_id
		WHERE a.name = 'ledger-bot' AND v.state = 'ACTIVE' AND v.active_allowlist_id IS NOT NULL`); n != 1 {
		t.Fatal("the version is not ACTIVE with an allowlist")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.bundle_resources`); n != 4 {
		t.Fatalf("managed objects = %d, want connector, tool, agent and version", n)
	}
	_, err = s.Approve(ctx, as(f, "ravi"), p.ID)
	if !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("second approve: %v", err)
	}
	again, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if again.ID != uuid.Nil || len(again.Steps) != 0 {
		t.Fatalf("replan after apply = %+v", again)
	}
}

func TestTheSubmitterCannotApprove(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "rita"), req(ledgerDoc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "rita"), p.ID)
	ok(t, err)
	_, err = s.Approve(ctx, as(f, "rita"), p.ID)
	wantCode(t, err, bundle.CodeSamePrincipal)
	_, err = s.Approve(ctx, as(f, "erin"), p.ID) // an editor is not an approver
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("editor approve: %v", err)
	}
	_, err = s.Approve(ctx, as(f, "ravi"), p.ID)
	ok(t, err)
}

func TestAStaleChangeSetRunsNothing(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	ok(t, f.Exec("alice", `UPDATE eacp.principals SET disabled_at = now(), disable_reason = 'left'
		WHERE name = 'carol'`))
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	wantCode(t, err, bundle.CodeStale)
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`); n != 0 {
		t.Fatal("a stale change set created the connector")
	}

	// Stale at approval: an operator quarantines the new version meanwhile.
	f2, s2 := setup(t)
	p, err = s2.Plan(ctx, as(f2, "erin"), req(ledgerDoc))
	ok(t, err)
	_, err = s2.Submit(ctx, as(f2, "erin"), p.ID)
	ok(t, err)
	ok(t, f2.Exec("otto", `UPDATE eacp.agent_versions SET state = 'QUARANTINED', state_reason = 'incident'`))
	_, err = s2.Approve(ctx, as(f2, "rita"), p.ID)
	wantCode(t, err, bundle.CodeStale)
}

func TestAFailingStepRollsBackTheStage(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	// Native idempotency without a key field: the contract CHECK refuses it.
	doc := strings.Replace(ledgerDoc, `"idempotency_mode": "none"`, `"idempotency_mode": "native"`, 1)
	p, err := s.Plan(ctx, as(f, "erin"), req(doc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	be := wantCode(t, err, bundle.CodeStepFailed)
	if be.Address != "contract.ledger.post_entry" || !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("step failure = %+v", be)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.connectors WHERE name = 'ledger'`); n != 0 {
		t.Fatal("the stage was not rolled back")
	}
	got, err := s.Get(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	if got.State != "PLANNED" {
		t.Fatalf("state after a failed submit = %s", got.State)
	}
}

func TestRejectAndTheOpenChangeSetLock(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	_, err = s.Submit(ctx, as(f, "erin"), p.ID)
	ok(t, err)
	_, err = s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	wantCode(t, err, bundle.CodeOpen)
	_, err = s.Reject(ctx, as(f, "rita"), p.ID, " ")
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("reject without reason: %v", err)
	}
	cs, err := s.Reject(ctx, as(f, "rita"), p.ID, "wrong owner")
	ok(t, err)
	if cs.State != "REJECTED" || cs.CloseReason != "wrong owner" {
		t.Fatalf("rejected = %+v", cs)
	}
	// The proposals stay inert; a new plan starts from what the submit created.
	next, err := s.Plan(ctx, as(f, "erin"), req(ledgerDoc))
	ok(t, err)
	if next.ID == uuid.Nil {
		t.Fatal("no plan after rejection")
	}
}

func TestAnObjectBelongsToOneBundle(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	apply(t, f, s, ledgerDoc)
	var conn uuid.UUID
	ok(t, storage.InTenantTx(ctx, f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM eacp.connectors WHERE name = 'ledger'`).Scan(&conn)
	}))
	other := `{"connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger"}},
		"imports": [{"to": "connector.ledger", "id": "` + conn.String() + `"}]}`
	_, err := s.Plan(ctx, as(f, "erin"), bundle.Request{Bundle: "finance", Desired: json.RawMessage(other)})
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if be.Findings[0].Kind != bundle.KindUnmanaged || !strings.Contains(be.Findings[0].Detail, "ledger") {
		t.Fatalf("findings = %+v", be.Findings)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Submit|Stale|Failing|Reject|OneBundle'`
Expected: FAIL to compile: `s.Submit undefined`.

- [ ] **Step 3: Implement `execute.go`**

`internal/bundle/execute.go`:

```go
package bundle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

type changeSetRow struct {
	id, bundleID uuid.UUID
	state        string
	submittedBy  *uuid.UUID
	hasApprove   bool
}

// lock locks the change set row first: every later write of the stage
// serializes behind it.
func lock(ctx context.Context, tx pgx.Tx, id uuid.UUID) (changeSetRow, error) {
	var r changeSetRow
	err := tx.QueryRow(ctx, `SELECT cs.id, cs.bundle_id, cs.state, cs.submitted_by,
		EXISTS (SELECT 1 FROM eacp.change_set_steps s
		         WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = 'approve')
		FROM eacp.change_sets cs WHERE cs.id = $1 FOR UPDATE`, id).
		Scan(&r.id, &r.bundleID, &r.state, &r.submittedBy, &r.hasApprove)
	return r, err
}

// Submit runs the submit-stage steps as a. Without approve-stage steps the
// change set is then APPLIED; otherwise it is SUBMITTED and sealed, and
// waits for a second person.
func (s *Service) Submit(ctx context.Context, a registry.Actor, id uuid.UUID) (ChangeSet, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		cs, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if cs.state != "PLANNED" {
			return conflict("change set %s is %s, not PLANNED", id, cs.state)
		}
		next := "APPLIED"
		if cs.hasApprove {
			next = "SUBMITTED"
		}
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = $2 WHERE id = $1`, id, next); err != nil {
			return err
		}
		if err := run(ctx, tx, cs, StageSubmit); err != nil {
			return err
		}
		if next == "SUBMITTED" {
			_, err = tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		}
		return err
	})
	if err != nil {
		return ChangeSet{}, err
	}
	return s.Get(ctx, a, id)
}

// Approve runs the approve-stage steps as a, who must not be the submitter.
func (s *Service) Approve(ctx context.Context, a registry.Actor, id uuid.UUID) (ChangeSet, error) {
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		cs, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if cs.state != "SUBMITTED" {
			return conflict("change set %s is %s, not SUBMITTED", id, cs.state)
		}
		if cs.submittedBy != nil && *cs.submittedBy == a.PrincipalID {
			return &Error{Code: CodeSamePrincipal, Msg: "the submitter cannot approve its own change set (two-person rule)"}
		}
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_sets SET state = 'APPLIED' WHERE id = $1`, id); err != nil {
			return err
		}
		return run(ctx, tx, cs, StageApprove)
	})
	if err != nil {
		return ChangeSet{}, err
	}
	return s.Get(ctx, a, id)
}

// Reject closes an open change set. Proposals its submission made stay
// inert: nothing activates them without a second person.
func (s *Service) Reject(ctx context.Context, a registry.Actor, id uuid.UUID, reason string) (ChangeSet, error) {
	if strings.TrimSpace(reason) == "" {
		return ChangeSet{}, invalid("rejecting a change set requires a reason")
	}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		cs, err := lock(ctx, tx, id)
		if err != nil {
			return err
		}
		if cs.state != "PLANNED" && cs.state != "SUBMITTED" {
			return conflict("change set %s is %s", id, cs.state)
		}
		_, err = tx.Exec(ctx, `UPDATE eacp.change_sets SET state = 'REJECTED', close_reason = $2 WHERE id = $1`,
			id, reason)
		return err
	})
	if err != nil {
		return ChangeSet{}, err
	}
	return s.Get(ctx, a, id)
}

// run executes a stage's steps in order and records what each produced.
func run(ctx context.Context, tx pgx.Tx, cs changeSetRow, stage string) error {
	steps, err := loadSteps(ctx, tx, cs.id)
	if err != nil {
		return err
	}
	ids := map[string]uuid.UUID{}
	produced := map[int]uuid.UUID{}
	for _, st := range steps {
		if st.ObjectID == nil {
			continue
		}
		produced[st.Ordinal] = *st.ObjectID
		if st.Op == OpCreate || st.Op == OpImport {
			ids[st.Address] = *st.ObjectID
		}
	}
	rtx := registry.Tx{Tx: tx}
	for _, st := range steps {
		if st.Stage != stage {
			continue
		}
		obj, err := apply(ctx, rtx, st, ids, produced)
		if err != nil {
			mapped := registry.MapErr(err)
			return &Error{Code: CodeStepFailed, Address: st.Address, Err: mapped,
				Msg: fmt.Sprintf("step %d (%s %s): %v", st.Ordinal, st.Op, st.Address, mapped)}
		}
		if _, err := tx.Exec(ctx, `UPDATE eacp.change_set_steps SET object_id = $3
			WHERE change_set_id = $1 AND ordinal = $2`, cs.id, st.Ordinal, obj); err != nil {
			return err
		}
		produced[st.Ordinal] = obj
		if st.Op != OpCreate && st.Op != OpImport {
			continue
		}
		ids[st.Address] = obj
		if kind, _, _ := strings.Cut(st.Address, "."); kind == "connector" || kind == "tool" || kind == "agent" ||
			kind == "version" {
			if _, err := tx.Exec(ctx, `INSERT INTO eacp.bundle_resources
				(tenant_id, bundle_id, address, kind, object_id, change_set_id)
				VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5)
				ON CONFLICT (tenant_id, bundle_id, address)
				DO UPDATE SET object_id = EXCLUDED.object_id, change_set_id = EXCLUDED.change_set_id`,
				cs.bundleID, st.Address, kind, obj, cs.id); err != nil {
				return err
			}
		}
	}
	return nil
}

var errPayload = errors.New("the step's payload is incomplete")

// apply makes one step's registry write and returns the object it produced
// or acted on.
func apply(ctx context.Context, rtx registry.Tx, st Step, ids map[string]uuid.UUID,
	produced map[int]uuid.UUID) (uuid.UUID, error) {
	p := st.Payload
	parent := func() (uuid.UUID, error) {
		if p.ParentID != nil {
			return *p.ParentID, nil
		}
		if id, ok := ids[p.Parent]; ok {
			return id, nil
		}
		return uuid.Nil, fmt.Errorf("parent %q was not created by this change set", p.Parent)
	}
	proposal := func() (uuid.UUID, error) {
		if id, ok := produced[p.ProposalStep]; ok {
			return id, nil
		}
		return uuid.Nil, fmt.Errorf("proposal step %d did not run", p.ProposalStep)
	}
	kind, _, _ := strings.Cut(st.Address, ".")
	switch st.Op + " " + kind {
	case "create connector":
		if p.Connector == nil {
			return uuid.Nil, errPayload
		}
		c, err := rtx.RegisterConnector(ctx, *p.Connector)
		return c.ID, err
	case "create tool":
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		return rtx.RegisterTool(ctx, pid, p.Name)
	case "create agent":
		if p.Agent == nil {
			return uuid.Nil, errPayload
		}
		ag, err := rtx.RegisterAgent(ctx, *p.Agent)
		return ag.ID, err
	case "create version":
		if p.Version == nil {
			return uuid.Nil, errPayload
		}
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		v, err := rtx.RegisterVersion(ctx, pid, *p.Version)
		return v.ID, err
	case "propose contract":
		if p.Contract == nil {
			return uuid.Nil, errPayload
		}
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		return rtx.ProposeContract(ctx, pid, *p.Contract)
	case "propose allowlist":
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		return rtx.ProposeAllowlist(ctx, pid, p.Tools)
	case "activate contract", "activate allowlist":
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		prop, err := proposal()
		if err != nil {
			return uuid.Nil, err
		}
		if kind == "contract" {
			return prop, rtx.ActivateContract(ctx, pid, prop)
		}
		return prop, rtx.ActivateAllowlist(ctx, pid, prop)
	case "transition version":
		pid, err := parent()
		if err != nil {
			return uuid.Nil, err
		}
		return pid, rtx.TransitionVersion(ctx, pid, p.To, p.Reason)
	case "revoke contract":
		if p.ObjectID == nil {
			return uuid.Nil, errPayload
		}
		return *p.ObjectID, rtx.RevokeContract(ctx, *p.ObjectID, p.Reason)
	}
	if st.Op == OpImport && p.ObjectID != nil {
		return *p.ObjectID, nil
	}
	return uuid.Nil, fmt.Errorf("unknown step %s %s", st.Op, st.Address)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race -count=1 ./internal/bundle/`
Expected: PASS (every test in the package).

- [ ] **Step 5: Commit**

```bash
git add internal/bundle/execute.go internal/bundle/service_test.go
git commit -m "feat(bundle): two-person submit and approve through the registry triggers"
```

---

### Task 7: Drift

**Files:**
- Create: `internal/bundle/drift.go`
- Modify: `internal/bundle/service_test.go` (add a test)

**Interfaces:**
- Consumes: `loadState`, `diff`, `Decode`, `Service.read`.
- Produces: `(*Service).Drift(ctx, a, bundle string) (Drift, error)`; `type Drift{Bundle string; ChangeSetID uuid.UUID; Entries []DriftEntry}`; `type DriftEntry{Address, Status, Detail string}`; statuses `DriftInSync`, `DriftModified`, `DriftMissing`, `DriftUnmanagedReference`.

- [ ] **Step 1: Write the failing test**

Append to `internal/bundle/service_test.go`:

```go
func TestDriftObservesAndNeverChanges(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	_, err := s.Drift(ctx, as(f, "audra"), "ledger")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("drift before any apply: %v", err)
	}
	applied := apply(t, f, s, ledgerDoc)
	status := func() map[string]string {
		t.Helper()
		d, err := s.Drift(ctx, as(f, "audra"), "ledger")
		ok(t, err)
		if d.ChangeSetID != applied.ID {
			t.Fatalf("drift of %s, want %s", d.ChangeSetID, applied.ID)
		}
		out := map[string]string{}
		for _, e := range d.Entries {
			out[e.Address] = e.Status
		}
		return out
	}
	for addr, st := range status() {
		if st != bundle.DriftInSync {
			t.Fatalf("%s is %s right after apply", addr, st)
		}
	}
	ok(t, f.Exec("otto", `UPDATE eacp.agent_versions SET state = 'SUSPENDED', state_reason = 'paused'`))
	if got := status()["version.ledger-bot"]; got != bundle.DriftModified {
		t.Fatalf("suspended version drift = %q", got)
	}
	ok(t, f.Exec("rita", `UPDATE eacp.agent_versions SET state = 'RETIRED', state_reason = 'gone'`))
	// Drift only reads: it adds no audit event and changes no change set.
	before := count(t, f, `SELECT count(*) FROM eacp.audit_events`)
	if got := status()["version.ledger-bot"]; got != bundle.DriftMissing {
		t.Fatalf("retired version drift = %q", got)
	}
	if after := count(t, f, `SELECT count(*) FROM eacp.audit_events`); after != before {
		t.Fatalf("drift wrote %d audit events", after-before)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.change_sets WHERE state <> 'APPLIED'`); n != 0 {
		t.Fatalf("drift left %d other change sets", n)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -race -count=1 ./internal/bundle/ -run TestDriftObservesAndNeverChanges`
Expected: FAIL to compile: `s.Drift undefined`.

- [ ] **Step 3: Implement `drift.go`**

`internal/bundle/drift.go`:

```go
package bundle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Drift statuses.
const (
	DriftInSync             = "in_sync"
	DriftModified           = "modified"
	DriftMissing            = "missing"
	DriftUnmanagedReference = "unmanaged_reference"
)

// DriftEntry is one address of a bundle and how the registry differs from
// the bundle's last applied change set.
type DriftEntry struct {
	Address string `json:"address"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
}

// Drift compares a bundle's last applied change set with the registry.
type Drift struct {
	Bundle      string       `json:"bundle"`
	ChangeSetID uuid.UUID    `json:"change_set_id"`
	Entries     []DriftEntry `json:"entries"`
}

// Drift reports, read-only, how the registry differs from what the bundle
// last applied. It never writes, blocks or remediates.
func (s *Service) Drift(ctx context.Context, a registry.Actor, bundle string) (Drift, error) {
	out := Drift{Bundle: bundle, Entries: []DriftEntry{}}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		var desired []byte
		err := tx.QueryRow(ctx, `SELECT cs.id, cs.desired FROM eacp.change_sets cs
			JOIN eacp.bundles b ON b.tenant_id = cs.tenant_id AND b.id = cs.bundle_id
			WHERE b.name = $1 AND cs.state = 'APPLIED'
			ORDER BY COALESCE(cs.approved_at, cs.submitted_at) DESC LIMIT 1`, bundle).Scan(&out.ChangeSetID, &desired)
		if errors.Is(err, pgx.ErrNoRows) {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "bundle " + bundle + " has no applied change set"}
		}
		if err != nil {
			return err
		}
		doc, err := Decode(desired)
		if err != nil {
			return err
		}
		st, err := loadState(ctx, tx, bundle)
		if err != nil {
			return err
		}
		out.Entries = driftEntries(st, diff(bundle, uuid.Nil, doc, st, false))
		return nil
	})
	return out, err
}

func driftEntries(st State, d Diff) []DriftEntry {
	by := map[string]DriftEntry{}
	for addr := range st.Managed {
		by[addr] = DriftEntry{Address: addr, Status: DriftInSync}
	}
	set := func(addr, status, detail string) {
		if e, ok := by[addr]; ok && e.Status != DriftInSync {
			return
		}
		by[addr] = DriftEntry{Address: addr, Status: status, Detail: detail}
	}
	for _, f := range d.Findings {
		switch f.Kind {
		case KindOrphan:
		case KindUnresolvedReference:
			set(f.Address, DriftMissing, f.Detail)
		case KindUnmanagedReference:
			set(f.Address, DriftUnmanagedReference, f.Detail)
		default:
			set(f.Address, DriftModified, f.Detail)
		}
	}
	for _, s := range d.Steps {
		set(s.Address, DriftModified, fmt.Sprintf("converging would %s %s", s.Op, s.Address))
	}
	// A managed version that was retired or revoked is gone, not modified.
	for addr, id := range st.Managed {
		kind, name, _ := strings.Cut(addr, ".")
		if kind != "version" {
			continue
		}
		for _, v := range st.Agents[name].Versions {
			if v.ID == id && (v.State == registry.StateRetired || v.State == registry.StateRevoked) {
				by[addr] = DriftEntry{Address: addr, Status: DriftMissing,
					Detail: fmt.Sprintf("version %d is %s", v.Number, v.State)}
			}
		}
	}
	out := make([]DriftEntry, 0, len(by))
	for _, e := range by {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b DriftEntry) int { return strings.Compare(a.Address, b.Address) })
	return out
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -race -count=1 ./internal/bundle/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/bundle/drift.go internal/bundle/service_test.go
git commit -m "feat(bundle): read-only drift against the last applied change set"
```

---

### Task 8: HTTP API

**Files:**
- Create: `internal/api/bundle.go`
- Modify: `internal/api/api.go` (Server struct, `New`, `Register`, `finish`)
- Test: `internal/api/bundle_test.go`

**Interfaces:**
- Consumes: `bundle.New`, `Request`, `ChangeSet`, `Error`, the `Code*` constants, `Plan`/`Get`/`List`/`Bundles`/`Submit`/`Approve`/`Reject`/`Drift`.
- Produces: the routes `POST /v1/change-sets`, `GET /v1/change-sets`, `GET /v1/change-sets/{id}`, `POST /v1/change-sets/{id}/submit|approve|reject`, `GET /v1/bundles`, `GET /v1/bundles/{name}/drift`.

- [ ] **Step 1: Write the failing API tests**

`internal/api/bundle_test.go`:

```go
package api_test

import (
	"encoding/json"
	"testing"

	"eacp/internal/storage/pgtest"
)

const bundleDoc = `{
 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "ledger",
   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
     "max_attempts": 3}}}}},
 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "high",
   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:aaa111"},
   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`

func TestChangeSetAPI(t *testing.T) {
	h := newHarness(t)
	plan := map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc)}

	code, body := h.as("carol", "POST", "/v1/change-sets", plan)
	h.want(403, code, body)
	code, body = h.as("erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc), "dry_run": true})
	h.want(200, code, body)
	if body["id"] != nil || len(body["steps"].([]any)) != 9 {
		t.Fatalf("dry run = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets", plan)
	h.want(201, code, body)
	id, _ := body["id"].(string)

	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/submit", nil)
	h.want(200, code, body)
	if body["state"] != "SUBMITTED" {
		t.Fatalf("submit = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(403, code, body) // not an approver (and the submitter)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(200, code, body)
	if body["state"] != "APPLIED" {
		t.Fatalf("approve = %v", body)
	}

	code, body = h.as("audra", "GET", "/v1/change-sets?bundle=ledger", nil)
	h.want(200, code, body)
	if cs, _ := body["change_sets"].([]any); len(cs) != 1 {
		t.Fatalf("list = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/bundles", nil)
	h.want(200, code, body)
	code, body = h.as("audra", "GET", "/v1/bundles/ledger/drift", nil)
	h.want(200, code, body)
	if entries, _ := body["entries"].([]any); len(entries) != 4 {
		t.Fatalf("drift = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/bundles/nope/drift", nil)
	h.want(404, code, body)
}

func TestChangeSetErrorCodes(t *testing.T) {
	h := newHarness(t)
	plan := map[string]any{"bundle": "ledger", "desired": json.RawMessage(bundleDoc)}

	bad := map[string]any{"bundle": "ledger", "desired": json.RawMessage(`{"agents": {"x": {"password": "p"}}}`)}
	code, body := h.as("erin", "POST", "/v1/change-sets", bad)
	h.want(400, code, body)

	blocked := map[string]any{"bundle": "ledger",
		"desired": json.RawMessage(`{"agents": {"bot": {"display_name": "B", "environment": "production",
			"risk_class": "low", "owner": {"principal": "nobody"}, "version": {"runtime": "go", "code_ref": "g:1"}}}}`)}
	code, body = h.as("erin", "POST", "/v1/change-sets", blocked)
	h.want(422, code, body)
	if body["error"] != "plan_blocked" || body["findings"] == nil {
		t.Fatalf("blocked = %v", body)
	}

	code, body = h.as("rita", "POST", "/v1/change-sets", plan)
	h.want(201, code, body)
	id := body["id"].(string)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/submit", nil)
	h.want(200, code, body)
	code, body = h.as("rita", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(403, code, body)
	if body["error"] != "same_principal" {
		t.Fatalf("self-approval = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets", plan)
	h.want(409, code, body)
	if body["error"] != "change_set_open" {
		t.Fatalf("open = %v", body)
	}
	code, body = h.as("erin", "POST", "/v1/change-sets/"+id+"/reject", map[string]any{"reason": "redo"})
	h.want(200, code, body)
	code, body = h.as("erin", "POST", "/v1/change-sets/not-a-uuid/submit", nil)
	h.want(400, code, body)
}

func TestChangeSetsOfOtherTenantsAreNotFound(t *testing.T) {
	h := newHarness(t)
	code, body := h.as("erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger",
		"desired": json.RawMessage(bundleDoc)})
	h.want(201, code, body)
	id := body["id"].(string)
	stranger := h.principalIn(pgtest.TenantB, "eve", "registry_approver", "auditor")
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/change-sets/" + id}, {"POST", "/v1/change-sets/" + id + "/submit"},
		{"POST", "/v1/change-sets/" + id + "/approve"}, {"GET", "/v1/bundles/ledger/drift"},
	} {
		code, body := h.do(stranger, c.method, c.path, nil)
		h.want(404, code, body)
	}
	code, body = h.do(stranger, "GET", "/v1/change-sets", nil)
	h.want(200, code, body)
	if cs, _ := body["change_sets"].([]any); len(cs) != 0 {
		t.Fatalf("tenant B lists %v", cs)
	}
}
```

Before running, read `internal/api/isolation_test.go:18` (`principalIn`) and `internal/api/api_test.go:142` (`do`) and adjust the calls to their exact signatures (for example, whether `principalIn` returns a key and takes roles variadically).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./internal/api/ -run 'ChangeSet'`
Expected: FAIL with 404s (the routes don't exist yet).

- [ ] **Step 3: Implement `bundle.go` and wire it in**

`internal/api/bundle.go`:

```go
package api

import (
	"net/http"
	"strconv"

	"eacp/internal/bundle"
	"eacp/internal/identity"
)

// Governance-as-Code (ADR-026). Planning and submitting are open to those
// who write the registry; approval to registry approvers and admins (the
// registry triggers still check each step's own role).
var (
	bundleWriter   = []string{"registry_editor", "registry_approver", "admin"}
	bundleApprover = []string{"registry_approver", "admin"}
	bundleReader   = []string{"registry_editor", "registry_approver", "admin", "auditor"}
)

func (s *Server) registerBundles(mux *http.ServeMux) {
	mux.Handle("POST /v1/change-sets", s.principal(bundleWriter, s.planChangeSet))
	mux.Handle("GET /v1/change-sets", s.principal(bundleReader, s.listChangeSets))
	mux.Handle("GET /v1/change-sets/{id}", s.principal(bundleReader, s.getChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/submit", s.principal(bundleWriter, s.submitChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/approve", s.principal(bundleApprover, s.approveChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/reject", s.principal(bundleWriter, s.rejectChangeSet))
	mux.Handle("GET /v1/bundles", s.principal(bundleReader, s.listBundles))
	mux.Handle("GET /v1/bundles/{name}/drift", s.principal(bundleReader, s.bundleDrift))
}

// bundleStatus is the HTTP status of a change-set error code.
func bundleStatus(code string) int {
	switch code {
	case bundle.CodePlanBlocked, bundle.CodeStepFailed:
		return http.StatusUnprocessableEntity
	case bundle.CodeSamePrincipal:
		return http.StatusForbidden
	default: // change_set_stale, change_set_open
		return http.StatusConflict
	}
}

func (s *Server) planChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in bundle.Request
	if err := decode(r, &in); err != nil {
		return err
	}
	cs, err := s.bundles.Plan(r.Context(), actor(c), in)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if cs.State != "" {
		code = http.StatusCreated
	}
	writeJSON(w, code, cs)
	return nil
}

func (s *Server) listChangeSets(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return badRequest{"limit must be a number"}
		}
		limit = n
	}
	list, err := s.bundles.List(r.Context(), actor(c), r.URL.Query().Get("bundle"), limit)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"change_sets": list})
	}
	return err
}

func (s *Server) getChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Get(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) submitChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Submit(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) approveChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Approve(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) rejectChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	cs, err := s.bundles.Reject(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) listBundles(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	list, err := s.bundles.Bundles(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"bundles": list})
	}
	return err
}

func (s *Server) bundleDrift(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	d, err := s.bundles.Drift(r.Context(), actor(c), r.PathValue("name"))
	if err == nil {
		writeJSON(w, http.StatusOK, d)
	}
	return err
}
```

In `internal/api/api.go`:

1. Import `"eacp/internal/bundle"`.
2. Add `bundles *bundle.Service` to `Server` next to `fleet *fleet.Service`.
3. In `New`, add `bundles: bundle.New(pool),` next to `fleet: fleet.New(pool),`.
4. In `Register`, add `s.registerBundles(mux)` after `s.registerFleet(mux)`.
5. In `finish`, handle a `*bundle.Error` **before** the `*registry.Error` case (a step failure wraps a registry error):

```go
	var br badRequest
	var be *bundle.Error
	var re *registry.Error
	switch {
	case errors.As(err, &br):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid", "detail": br.msg})
	case errors.As(err, &be):
		writeJSON(w, bundleStatus(be.Code), be)
	case errors.As(err, &re):
```

A bad bundle document decodes into `registry.ErrInvalid`, which gives 400 through the existing mapping. That is what `TestChangeSetErrorCodes` expects for the `password` field.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go vet ./... && go test -race -count=1 ./internal/api/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/bundle.go internal/api/bundle_test.go internal/api/api.go
git commit -m "feat(api): change-set and bundle routes (ADR-026)"
```

---

### Task 9: `eacpctl bundle`

**Files:**
- Create: `cmd/eacpctl/bundle.go`
- Modify: `cmd/eacpctl/client.go` (split `call` into `request` + print)
- Modify: `cmd/eacpctl/main.go` (dispatch, usage comment and `usage` text)
- Modify: `go.mod` (make `go.yaml.in/yaml/v3 v3.0.5` a direct requirement)
- Test: `cmd/eacpctl/bundle_test.go`

**Interfaces:**
- Consumes: the API routes of Task 8.
- Produces: `runBundle(ctx, args, getenv, out) error`; `loadBundle(dir, target string, vars map[string]string) (loadedBundle, error)`; `request(ctx, getenv, method, path string, body any) ([]byte, error)`.

- [ ] **Step 1: Write the failing CLI tests**

`cmd/eacpctl/bundle_test.go`:

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const eacpYML = `bundle:
  name: procurement
targets:
  dev:
    default: true
  prod:
    tenant: 11111111-1111-4111-8111-111111111111
    variables:
      erp_endpoint: https://erp.example.com
variables:
  erp_endpoint:
    default: http://fakeerp:8090
  code_ref: {}
resources:
  connectors:
    erp:
      protocol: http
      endpoint: ${var.erp_endpoint}
      secret_ref: erp-token
      tools:
        create_po:
          contract: {side_effects: [READ_ONLY], idempotency_mode: none, reconciliation_lookup: none,
                     reconciliation_consistency: none, proof_standard: none, max_attempts: 3}
import:
  - {to: connector.erp, id: 0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01}
`

const agentsYML = `resources:
  agents:
    buyer:
      display_name: Buyer
      environment: production
      risk_class: high
      owner: {group: procurement}
      version: {runtime: python, code_ref: "${var.code_ref}"}
      allowlist: [erp.create_po]
      state: ACTIVE
`

func writeBundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadBundleResolvesTargetsAndVariables(t *testing.T) {
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})
	b, err := loadBundle(dir, "prod", map[string]string{"code_ref": "git:abc"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b.desired)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	conn := got["connectors"].(map[string]any)["erp"].(map[string]any)
	agent := got["agents"].(map[string]any)["buyer"].(map[string]any)
	if b.name != "procurement" || conn["endpoint"] != "https://erp.example.com" ||
		agent["version"].(map[string]any)["code_ref"] != "git:abc" {
		t.Fatalf("resolved = %s", raw)
	}
	if im := got["imports"].([]any); len(im) != 1 || im[0].(map[string]any)["to"] != "connector.erp" {
		t.Fatalf("imports = %v", got["imports"])
	}
	b, err = loadBundle(dir, "", map[string]string{"code_ref": "git:abc"}) // the default target
	if err != nil || b.targetName != "dev" {
		t.Fatalf("default target = %q, %v", b.targetName, err)
	}

	for name, tc := range map[string]struct {
		files  map[string]string
		target string
		vars   map[string]string
		want   string
	}{
		"missing variable":   {map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML}, "dev", nil, "code_ref"},
		"undeclared --var":   {map[string]string{"eacp.yml": eacpYML}, "dev", map[string]string{"nope": "x"}, "nope"},
		"unknown target":     {map[string]string{"eacp.yml": eacpYML}, "qa", nil, "qa"},
		"unknown field":      {map[string]string{"eacp.yml": eacpYML + "secrets: {a: b}\n"}, "dev", nil, "secrets"},
		"duplicate resource": {map[string]string{"eacp.yml": eacpYML, "resources/a.yml": agentsYML, "resources/b.yml": agentsYML}, "dev", map[string]string{"code_ref": "x"}, "buyer"},
		"no name":            {map[string]string{"eacp.yml": "resources: {}\n"}, "", nil, "bundle.name"},
	} {
		dir := writeBundle(t, tc.files)
		if _, err := loadBundle(dir, tc.target, tc.vars); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
}

func TestBundleCommands(t *testing.T) {
	env, got := recordingAPI(t)
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})
	const id = "0b9f0e6a-4c8e-4a55-9d1e-4f2f6f1f9a01"
	for _, tc := range []struct {
		args   []string
		method string
		uri    string
		body   map[string]any
	}{
		{[]string{"plan", "-C", dir, "--var", "code_ref=git:1", "--dry-run", "--prune"}, "POST", "/v1/change-sets", nil},
		{[]string{"approve", id}, "POST", "/v1/change-sets/" + id + "/approve", nil},
		{[]string{"reject", id, "--reason", "redo"}, "POST", "/v1/change-sets/" + id + "/reject",
			map[string]any{"reason": "redo"}},
		{[]string{"status", id}, "GET", "/v1/change-sets/" + id, nil},
		{[]string{"list", "-C", dir}, "GET", "/v1/change-sets?bundle=procurement", nil},
		{[]string{"drift", "-C", dir}, "GET", "/v1/bundles/procurement/drift", nil},
	} {
		*got = nil
		out, err := runWith(t, env, append([]string{"bundle"}, tc.args...)...)
		if err != nil || !strings.Contains(out, `"ok": true`) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
		if len(*got) != 1 || (*got)[0].method != tc.method || (*got)[0].uri != tc.uri {
			t.Fatalf("%v sent %+v", tc.args, *got)
		}
		if tc.body != nil && !reflect.DeepEqual((*got)[0].body, tc.body) {
			t.Fatalf("%v body = %v", tc.args, (*got)[0].body)
		}
		if tc.args[0] == "plan" {
			b := (*got)[0].body
			if b["bundle"] != "procurement" || b["dry_run"] != true || b["prune"] != true || b["desired"] == nil {
				t.Fatalf("plan body = %v", b)
			}
		}
	}
	// validate is offline.
	*got = nil
	out, err := runWith(t, env, "bundle", "validate", "-C", dir, "--var", "code_ref=git:1")
	if err != nil || len(*got) != 0 || !strings.Contains(out, `"buyer"`) {
		t.Fatalf("validate: %v, %d calls, %s", err, len(*got), out)
	}
	for _, args := range [][]string{
		{"bundle"}, {"bundle", "explode"}, {"bundle", "approve"}, {"bundle", "approve", "not-a-uuid"},
		{"bundle", "reject", id}, {"bundle", "plan", "-C", dir}, // code_ref has no value
	} {
		if _, err := runWith(t, env, args...); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestBundleDeployPlansThenSubmitsAndChecksTheTenant(t *testing.T) {
	const id = "7c1d2e3f-0000-4000-8000-000000000001"
	var calls []string
	tenant := "11111111-1111-4111-8111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/me":
			_, _ = w.Write([]byte(`{"tenant_id": "` + tenant + `"}`))
		case "/v1/change-sets":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "` + id + `", "state": "PLANNED", "steps": []}`))
		default:
			_, _ = w.Write([]byte(`{"id": "` + id + `", "state": "SUBMITTED"}`))
		}
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"EACP_API_URL": srv.URL, "EACP_API_KEY": "k"}
	dir := writeBundle(t, map[string]string{"eacp.yml": eacpYML, "resources/agents.yml": agentsYML})

	if _, err := runWith(t, env, "bundle", "deploy", "-C", dir, "-t", "prod", "--var", "code_ref=git:1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/me", "POST /v1/change-sets", "POST /v1/change-sets/" + id + "/submit"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
	tenant = "22222222-2222-4222-8222-222222222222"
	calls = nil
	_, err := runWith(t, env, "bundle", "deploy", "-C", dir, "-t", "prod", "--var", "code_ref=git:1")
	if err == nil || !strings.Contains(err.Error(), "tenant") || len(calls) != 1 {
		t.Fatalf("mismatched tenant: %v, calls %v", err, calls)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race -count=1 ./cmd/eacpctl/ -run Bundle`
Expected: FAIL to compile: `undefined: loadBundle`.

- [ ] **Step 3: Split `call` in `client.go`**

Replace `call` in `cmd/eacpctl/client.go` with:

```go
// request sends one API request with the caller's key and returns the raw
// response body. Non-2xx responses are errors carrying the status.
func request(ctx context.Context, getenv func(string) string, method, path string, body any) ([]byte, error) {
	key := getenv("EACP_API_KEY")
	if key == "" {
		return nil, errors.New("EACP_API_KEY: required")
	}
	base := getenv("EACP_API_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case json.RawMessage:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(fmt.Sprintf(`{"status": %d}`, resp.StatusCode))
	}
	return raw, nil
}

// call sends one API request and prints the JSON response indented.
func call(ctx context.Context, getenv func(string) string, out io.Writer, method, path string, body any) error {
	raw, err := request(ctx, getenv, method, path, body)
	if err != nil {
		return err
	}
	return printJSON(out, raw)
}

func printJSON(out io.Writer, raw []byte) error {
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil {
		_, err := out.Write(raw)
		return err
	}
	pretty.WriteByte('\n')
	_, err := pretty.WriteTo(out)
	return err
}
```

This changes one behaviour: an empty 2xx body used to print `HTTP 204`. Check the existing tests with `grep -rn '"HTTP 2' cmd/eacpctl/`. If any test expects `HTTP 204`, keep that output: have `request` return `nil` for an empty body, and have `call` print `fmt.Fprintf(out, "HTTP %d\n", ...)`. That means `request` must also return the status code. Pick whichever keeps the existing tests green without editing them.

- [ ] **Step 4: Implement `bundle.go`**

`cmd/eacpctl/bundle.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

const bundleUsage = `usage:
  eacpctl bundle validate [-C DIR] [-t TARGET] [--var NAME=VALUE]...
  eacpctl bundle plan     [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune] [--dry-run]
  eacpctl bundle deploy   [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune]
  eacpctl bundle approve|status <change-set-id> [-C DIR] [-t TARGET]
  eacpctl bundle reject <change-set-id> --reason TEXT [-C DIR] [-t TARGET]
  eacpctl bundle list|drift [-C DIR] [-t TARGET]`

// bundleConfig is eacp.yml. Files under resources/*.yml hold only a
// resources: block, merged into it.
type bundleConfig struct {
	Bundle struct {
		Name string `yaml:"name"`
	} `yaml:"bundle"`
	Targets   map[string]bundleTarget   `yaml:"targets"`
	Variables map[string]bundleVariable `yaml:"variables"`
	Resources bundleResources           `yaml:"resources"`
	Import    []bundleImport            `yaml:"import"`
	Prune     bool                      `yaml:"prune"`
}

// bundleTarget is an environment: its API, the tenant its key must belong
// to, and variable values.
type bundleTarget struct {
	API       string            `yaml:"api"`
	Tenant    string            `yaml:"tenant"`
	Default   bool              `yaml:"default"`
	Variables map[string]string `yaml:"variables"`
}

type bundleVariable struct {
	Default     *string `yaml:"default"`
	Description string  `yaml:"description"`
}

type bundleResources struct {
	Connectors map[string]any `yaml:"connectors"`
	Agents     map[string]any `yaml:"agents"`
}

type bundleImport struct {
	To string `yaml:"to"`
	ID string `yaml:"id"`
}

type loadedBundle struct {
	name       string
	targetName string
	target     bundleTarget
	prune      bool
	desired    map[string]any
}

var varRE = regexp.MustCompile(`\$\{var\.([A-Za-z_][A-Za-z0-9_]*)\}`)

// errNoValue marks a variable without a value: fatal for validate, plan and
// deploy, which send the document, but not for list and drift, which need
// only the bundle's name.
var errNoValue = errors.New("variable has no value")

func decodeYAMLFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// loadBundle reads dir/eacp.yml and dir/resources/*.yml, picks the target
// (the named one, else the default one, else the only one), and resolves
// ${var.NAME} from --var, then the target, then the variable's default.
func loadBundle(dir, targetName string, vars map[string]string) (loadedBundle, error) {
	var cfg bundleConfig
	if err := decodeYAMLFile(filepath.Join(dir, "eacp.yml"), &cfg); err != nil {
		return loadedBundle{}, err
	}
	if cfg.Bundle.Name == "" {
		return loadedBundle{}, errors.New("eacp.yml: bundle.name is required")
	}
	res := bundleResources{Connectors: map[string]any{}, Agents: map[string]any{}}
	merge := func(dst, src map[string]any, kind, file string) error {
		for k, v := range src {
			if _, dup := dst[k]; dup {
				return fmt.Errorf("%s: %s %q is declared twice", file, kind, k)
			}
			dst[k] = v
		}
		return nil
	}
	if err := merge(res.Connectors, cfg.Resources.Connectors, "connector", "eacp.yml"); err != nil {
		return loadedBundle{}, err
	}
	if err := merge(res.Agents, cfg.Resources.Agents, "agent", "eacp.yml"); err != nil {
		return loadedBundle{}, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "resources", "*.yml"))
	if err != nil {
		return loadedBundle{}, err
	}
	slices.Sort(files)
	for _, f := range files {
		var rf struct {
			Resources bundleResources `yaml:"resources"`
		}
		if err := decodeYAMLFile(f, &rf); err != nil {
			return loadedBundle{}, err
		}
		if err := merge(res.Connectors, rf.Resources.Connectors, "connector", f); err != nil {
			return loadedBundle{}, err
		}
		if err := merge(res.Agents, rf.Resources.Agents, "agent", f); err != nil {
			return loadedBundle{}, err
		}
	}

	b := loadedBundle{name: cfg.Bundle.Name, prune: cfg.Prune}
	switch {
	case targetName != "":
		t, ok := cfg.Targets[targetName]
		if !ok {
			return b, fmt.Errorf("eacp.yml: no target %q", targetName)
		}
		b.target, b.targetName = t, targetName
	default:
		for name, t := range cfg.Targets {
			if t.Default || len(cfg.Targets) == 1 {
				if b.targetName != "" {
					return b, errors.New("eacp.yml: more than one default target: pass -t")
				}
				b.target, b.targetName = t, name
			}
		}
	}

	values := map[string]string{}
	for name, v := range cfg.Variables {
		if v.Default != nil {
			values[name] = *v.Default
		}
	}
	for _, layer := range []map[string]string{b.target.Variables, vars} {
		for k, v := range layer {
			if _, declared := cfg.Variables[k]; !declared {
				return b, fmt.Errorf("variable %q is not declared in eacp.yml", k)
			}
			values[k] = v
		}
	}

	desired := map[string]any{}
	if len(res.Connectors) > 0 {
		desired["connectors"] = res.Connectors
	}
	if len(res.Agents) > 0 {
		desired["agents"] = res.Agents
	}
	if len(cfg.Import) > 0 {
		imports := make([]any, 0, len(cfg.Import))
		for _, im := range cfg.Import {
			imports = append(imports, map[string]any{"to": im.To, "id": im.ID})
		}
		desired["imports"] = imports
	}
	resolved, err := substitute(desired, values)
	if err != nil {
		return b, err
	}
	b.desired = resolved.(map[string]any)
	return b, nil
}

func substitute(v any, values map[string]string) (any, error) {
	switch x := v.(type) {
	case string:
		var missing string
		out := varRE.ReplaceAllStringFunc(x, func(m string) string {
			name := varRE.FindStringSubmatch(m)[1]
			val, ok := values[name]
			if !ok {
				missing = name
				return m
			}
			return val
		})
		if missing != "" {
			return nil, fmt.Errorf("%w: %q: give it a default, set it in the target, or pass --var", errNoValue,
				missing)
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := substitute(e, values)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := substitute(e, values)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// targetEnv points the API client at the target's API, if it names one.
func targetEnv(getenv func(string) string, t bundleTarget) func(string) string {
	return func(k string) string {
		if k == "EACP_API_URL" && t.API != "" {
			return t.API
		}
		return getenv(k)
	}
}

// checkTenant refuses to plan against a tenant other than the target's.
func checkTenant(ctx context.Context, getenv func(string) string, b loadedBundle) error {
	if b.target.Tenant == "" {
		return nil
	}
	raw, err := request(ctx, getenv, "GET", "/v1/me", nil)
	if err != nil {
		return err
	}
	var me struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return err
	}
	if me.TenantID != b.target.Tenant {
		return fmt.Errorf("target %s is tenant %s, but EACP_API_KEY belongs to tenant %s", b.targetName,
			b.target.Tenant, me.TenantID)
	}
	return nil
}

func runBundle(ctx context.Context, args []string, getenv func(string) string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(bundleUsage)
	}
	cmd, rest := args[0], args[1:]
	var id string
	switch cmd {
	case "approve", "status", "reject":
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(bundleUsage)
		}
		if _, err := uuid.Parse(rest[0]); err != nil {
			return errors.New(bundleUsage)
		}
		id, rest = rest[0], rest[1:]
	case "validate", "plan", "deploy", "list", "drift":
	default:
		return errors.New(bundleUsage)
	}
	fs := newFlags("bundle " + cmd)
	dir := fs.String("C", ".", "bundle directory")
	target := fs.String("t", "", "target")
	prune := fs.Bool("prune", false, "retire or revoke what the bundle no longer declares")
	dryRun := fs.Bool("dry-run", false, "plan without recording a change set")
	reason := fs.String("reason", "", "why the change set is rejected")
	vars := map[string]string{}
	fs.Func("var", "NAME=VALUE", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return errors.New("--var takes NAME=VALUE")
		}
		vars[k] = val
		return nil
	})
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (cmd == "reject" && *reason == "") {
		return errors.New(bundleUsage)
	}

	// approve, status and reject need only an API: the bundle is optional.
	// list and drift need only its name and target.
	var b loadedBundle
	needsDocument := cmd == "validate" || cmd == "plan" || cmd == "deploy"
	if _, statErr := os.Stat(filepath.Join(*dir, "eacp.yml")); statErr == nil || id == "" {
		var err error
		b, err = loadBundle(*dir, *target, vars)
		if err != nil && id == "" && (needsDocument || !errors.Is(err, errNoValue)) {
			return err
		}
	}
	env := targetEnv(getenv, b.target)

	switch cmd {
	case "validate":
		raw, err := json.Marshal(map[string]any{"bundle": b.name, "target": b.targetName, "desired": b.desired})
		if err != nil {
			return err
		}
		return printJSON(out, raw)
	case "approve":
		return call(ctx, env, out, "POST", "/v1/change-sets/"+id+"/approve", nil)
	case "reject":
		return call(ctx, env, out, "POST", "/v1/change-sets/"+id+"/reject", map[string]any{"reason": *reason})
	case "status":
		return call(ctx, env, out, "GET", "/v1/change-sets/"+id, nil)
	case "list":
		return call(ctx, env, out, "GET", "/v1/change-sets?"+url.Values{"bundle": {b.name}}.Encode(), nil)
	case "drift":
		return call(ctx, env, out, "GET", "/v1/bundles/"+url.PathEscape(b.name)+"/drift", nil)
	}

	if err := checkTenant(ctx, env, b); err != nil {
		return err
	}
	body := map[string]any{"bundle": b.name, "desired": b.desired, "prune": *prune || b.prune}
	if cmd == "plan" && *dryRun {
		body["dry_run"] = true
	}
	raw, err := request(ctx, env, "POST", "/v1/change-sets", body)
	if err != nil {
		return err
	}
	if err := printJSON(out, raw); err != nil || cmd == "plan" {
		return err
	}
	var planned struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &planned); err != nil {
		return err
	}
	if planned.ID == "" {
		_, err := fmt.Fprintln(out, "no changes: the registry matches the bundle")
		return err
	}
	return call(ctx, env, out, "POST", "/v1/change-sets/"+planned.ID+"/submit", nil)
}
```

In `TestBundleCommands`, `plan … --prune` and `--dry-run` go into the body as `true`. The `recordingAPI` server always answers `{"ok":true}`, so for `deploy` use the dedicated test server in `TestBundleDeployPlansThenSubmitsAndChecksTheTenant`.

In `cmd/eacpctl/main.go`:
- add `case "bundle": return runBundle(ctx, args[1:], getenv, out)` to `run`, next to `case "fleet":`;
- add these lines to the doc comment and the `usage` constant, next to the fleet lines:

```text
//	eacpctl bundle validate|plan|deploy [-C DIR] [-t TARGET] [--var NAME=VALUE]... [--prune] [--dry-run]
//	eacpctl bundle approve|status|reject <change-set-id> [--reason TEXT]
//	eacpctl bundle list|drift [-C DIR] [-t TARGET]
//	                          Governance-as-Code: plan, two-person apply and drift (ADR-026)
```

- [ ] **Step 5: Make the YAML module a direct dependency**

Run: `go get go.yaml.in/yaml/v3@v3.0.5 && go mod tidy`
Expected: `go.mod` lists `go.yaml.in/yaml/v3 v3.0.5` in the direct `require` block; `go.sum` is unchanged or gains only its own lines.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go vet ./... && go test -race -count=1 ./cmd/eacpctl/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/eacpctl/bundle.go cmd/eacpctl/bundle_test.go cmd/eacpctl/client.go cmd/eacpctl/main.go go.mod go.sum
git commit -m "feat(eacpctl): bundle validate, plan, deploy, approve, reject and drift"
```

---

### Task 10: Demo step

**Files:**
- Modify: `test/demo/slice_c_test.go` (a new step before the secret scan)
- Modify: `docs/DEMO.md` (the Slice C step list)

**Interfaces:**
- Consumes: `d.must(want int, who, method, path string, body any) map[string]any`, `d.step`, `d.logf` from `test/demo/demo_test.go`; the API routes of Task 8.

- [ ] **Step 1: Add the step**

In `TestSliceCDemo`, replace

```go
	d.step("C7. Scan responses, logs and the database for the ERP and MCP credentials")
	d.secretScan()
```

with

```go
	d.step("C7. Governance-as-Code: plan a bundle, submit (erin), approve (rita), check drift")
	d.governanceAsCode()

	d.step("C8. Scan responses, logs and the database for the ERP and MCP credentials")
	d.secretScan()
```

and add to `test/demo/slice_c_test.go`:

```go
// governanceAsCode declares a ledger connector and a ledger agent as a
// bundle (ADR-026): a plan writes nothing, erin submits, rita approves, a
// replan is empty, and drift is in sync until an operator pauses the agent.
func (d *demo) governanceAsCode() {
	desired := json.RawMessage(`{
	 "connectors": {"ledger": {"protocol": "http", "endpoint": "http://fakeerp:8090", "secret_ref": "erp",
	   "tools": {"post_entry": {"contract": {"side_effects": ["READ_ONLY"], "idempotency_mode": "none",
	     "reconciliation_lookup": "none", "reconciliation_consistency": "none", "proof_standard": "none",
	     "max_attempts": 3}}}}},
	 "agents": {"ledger-bot": {"display_name": "Ledger bot", "environment": "production", "risk_class": "medium",
	   "owner": {"principal": "carol"}, "version": {"runtime": "python", "code_ref": "git:ledger-1"},
	   "allowlist": ["ledger.post_entry"], "state": "ACTIVE"}}}`)
	plan := d.must(201, "erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger", "desired": desired})
	id := plan["id"].(string)
	d.logf("plan %s: %d steps, base digest %s; nothing written yet", id, len(plan["steps"].([]any)),
		plan["base_digest"])
	d.must(200, "erin", "POST", "/v1/change-sets/"+id+"/submit", nil)
	if res := d.must(403, "erin", "POST", "/v1/change-sets/"+id+"/approve", nil); res["error"] == nil {
		d.t.Fatalf("self-approval = %v", res)
	}
	applied := d.must(200, "rita", "POST", "/v1/change-sets/"+id+"/approve", nil)
	d.logf("erin submitted, erin could not approve, rita approved: %s", applied["state"])
	again := d.must(200, "erin", "POST", "/v1/change-sets", map[string]any{"bundle": "ledger", "desired": desired})
	if len(again["steps"].([]any)) != 0 {
		d.t.Fatalf("replan after apply = %v", again)
	}
	drift := d.must(200, "audra", "GET", "/v1/bundles/ledger/drift", nil)
	d.logf("replan: no changes; drift: %d addresses in sync", len(drift["entries"].([]any)))
}
```

If `must` compares against the response code and fails the test on a mismatch, the `403` call is fine as written. If `must` only accepts 2xx, use `d.call("erin", "POST", ...)` and check the code yourself (see `test/demo/demo_test.go:262`).

- [ ] **Step 2: Update `docs/DEMO.md`**

In the Slice C step list, insert a row or bullet for step C7 matching the log lines above, and renumber the secret scan to C8.

- [ ] **Step 3: Compile the demo package**

Run: `go vet ./test/demo/`
Expected: no output. (The demo itself runs only with `EACP_DEMO=1` and compose; run `DEMO=C scripts/demo.sh` if the stack is available, and report whether you did.)

- [ ] **Step 4: Commit**

```bash
git add test/demo/slice_c_test.go docs/DEMO.md
git commit -m "test(demo): Governance-as-Code step in the Slice C demo"
```

---

### Task 11: ADR-026, plan, invariants and agent instructions

**Files:**
- Create: `docs/adr/ADR-026-governance-as-code.md`
- Modify: `docs/adr/README.md`, `docs/MASTER_PLAN.md`, `docs/INVARIANTS.md`, `AGENTS.md`

- [ ] **Step 1: Write ADR-026**

`docs/adr/ADR-026-governance-as-code.md`:

```markdown
# ADR-026 — Governance-as-Code: Bundles, Plans, Change Sets and Drift

Status: Accepted (Rev 1.0) · Phase 20 · Date: 2026-09-25

## Context

Registry owners want the governed estate (connectors, tools, contracts, agents, versions, allowlists)
in files under review in Git, with a plan they can read before anything changes. Terraform (plan, apply,
state, import, drift) and Databricks Asset Bundles (a bundle directory, targets, variables, validate and
deploy) are the models. EACP must not gain a second authority: PostgreSQL triggers decide every registry
write (ADR-003 §8), and two-person rules stay two-person.

## Decision

1. **A bundle is desired state, resolved by the client.** `eacpctl bundle` reads `eacp.yml` and
   `resources/*.yml`, picks a target (API URL, tenant, variable values), resolves `${var.NAME}` and sends
   one JSON document. The server accepts JSON only, decodes it strictly (an unknown field is refused, so a
   secret cannot ride along) and refuses any unresolved `${`. A bundle never contains a secret value;
   connectors carry `secret_ref` only.
2. **PostgreSQL is the state.** `eacp.bundle_resources` maps a bundle's addresses (`connector.erp`,
   `tool.erp.create_po`, `agent.buyer`, `version.buyer`) to objects. An object belongs to at most one
   bundle. There is no state file; the open change set (one per bundle) is the lock.
3. **A plan is recorded, sealed and never writes the registry.** The API diffs the document against the
   registry in one REPEATABLE READ snapshot and records a change set: its canonical document, ordered
   steps (`create`, `propose`, `activate`, `transition`, `revoke`, `import`), each in stage `submit` or
   `approve`, and the refs it read. PostgreSQL computes `desired_digest` and `base_digest` (over the refs
   and the bundle's managed objects) and journals `change_set.planned`. A blocking finding records nothing.
4. **Apply is two stages, two people.** Submit locks the change set, checks `base_digest` (stale:
   `40001`), runs the submit steps as the submitter through `registry.Tx`, the same SQL as the API, then
   seals `sealed_digest` over the refs plus every object it produced. Approve needs a registry approver or
   admin who is not the submitter, checks `sealed_digest`, and runs the activations and transitions. Each
   step still passes its own trigger (roles, ≠ creator, ≠ allowlist author, MCP definition pin, the
   one-`ACTIVE` release guard). A stage is one transaction: any failing step rolls it all back. A change
   set without approve steps applies at submit, as the same single-person writes do through the API.
5. **Nothing is deleted or replaced.** A change to an immutable field is `unsupported`. A managed object
   missing from the document is an `orphan`; with `prune` it becomes an existing lifecycle move only
   (retire a REGISTERED or SUSPENDED version, revoke a contract). An ACTIVE version is never retired by a
   bundle.
6. **Bundles do not bypass releases or containment.** Activating a version while another of the agent is
   ACTIVE is `requires_release` (ADR-018). A SUSPENDED or QUARANTINED version, or a quarantined MCP tool,
   is `contained`: the bundle never resumes or releases it (ADR-024, ADR-023). MCP tools are discovered,
   never declared; a bundle may pin the current definition in a contract.
7. **Drift observes.** `GET /v1/bundles/{name}/drift` compares the last applied document with the registry
   in a read-only snapshot and reports `in_sync`, `modified`, `missing` or `unmanaged_reference`. It never
   writes, blocks or remediates.

## Consequences

- Git review plus a recorded, digest-bound plan gives a reviewable path from a pull request to the
  registry, with every write still journaled by the registry and the change set journaled at each move.
- A plan made against one state cannot be applied to another: any change to a ref makes it stale.
- Upgrading a live agent stays a release; the bundle registers the new version and later adopts the
  promoted one by `code_ref`.
- The trust boundary is unchanged (ADR-003 §8): the guards stop application bugs, not a holder of a stolen
  `eacp_app` credential. A step's recorded object id is management metadata, never authority.

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Should a change set of single-person steps need approval? | No: it applies at submit, like the same API writes. A later ADR may tighten it. |
| Proposals of a rejected submission | They stay pending and inert; activating one still needs a second person. |
| Scope of the digest | The plan's refs and the bundle's managed objects, not the whole tenant. |
| Credentials in bundles | Excluded: they expire within 90 days and use bring-your-own-key hashes. |
| Principals, roles, policies and budgets | Phase 21, on the same engine. |
```

- [ ] **Step 2: Update the ADR index**

In `docs/adr/README.md`, add after the ADR-025 row:

```markdown
| [ADR-026](ADR-026-governance-as-code.md) | Governance-as-Code: Bundles, Plans, Change Sets and Drift | Accepted (Rev 1.0) | Phase 20 |
```

- [ ] **Step 3: Update the master plan**

In `docs/MASTER_PLAN.md`:

1. Before `# 94. Phase 20 — Agent SOC (Later)`, insert:

```markdown
# 93a. Phase 20 — Governance-as-Code (Later)

> **Status (2026-09-25): delivered.** Normative detail: [ADR-026](adr/ADR-026-governance-as-code.md) Rev 1.0. A bundle (`eacp.yml`, targets, variables) declares connectors, tools, contracts, agents, versions and allowlists. The API plans it in one snapshot into a change set whose digests PostgreSQL computes. Submit (one person) and approve (a second) run its steps through the same registry writes and triggers as the API; a stale change set runs nothing. Nothing is deleted, releases and containment are never bypassed, and drift is read-only.

---

# 93b. Phase 21 — Governance-as-Code: identity, policy and budgets (Later)

Principals, groups, memberships and role grants; governance policy versions; budget accounts and limit changes (raising stays two-person); FinOps soft limits and forward-only prices. They use the Phase 20 engine.

---
```

2. Renumber the Later phase headings: `Phase 20 — Agent SOC` → `Phase 22 — Agent SOC`, `Phase 21 — Kubernetes / HA` → `Phase 23 …`, `Phase 22 — JIT Credentials` → `Phase 24 …`, `Phase 23 — A2A & LLM Gateway` → `Phase 25 …`. Keep the § numbers.
3. Update the other phase references. Run `grep -n "Phase 2[0-3]" docs/MASTER_PLAN.md` and fix each one that means a shifted phase, including line ~284 (`JIT … Phase 22 (§96)` → `Phase 24 (§96)`), line ~2949 (`Phase 22 (JIT credentials)` → `Phase 24`) and the slice table (`Later — Operations & ecosystem Phase 18–23` → `Phase 18–25`).

- [ ] **Step 4: Update the invariants map**

In `docs/INVARIANTS.md`, add after the Phase 18 paragraph:

```markdown
Phase 20 adds Governance-as-Code (ADR-026): bundles planned into change sets, two-person submit and approve through the registry triggers, and read-only drift. It grants nothing a registry write through the API could not. Its tests join invariants 8 and 17. `internal/bundle/schema_test.go` also checks every change-set rule in raw SQL; `internal/bundle/plan_test.go` checks the ordered diff, releases, containment, imports and prune; `internal/bundle/service_test.go` checks staleness, atomic stages and drift.
```

Under `## 8`, add:

```markdown
- `internal/bundle` TestChangeSetsAreTenantIsolated — change sets, bundles and steps are tenant rows under RLS
- `internal/api` TestChangeSetsOfOtherTenantsAreNotFound — the change-set and drift API answers 404 across tenants
```

Under `## 17`, add:

```markdown
- `internal/bundle` TestClosedChangeSetsAreTerminalAndRejectionNeedsAReason — every change-set move is journaled with its actor, a rejection with its reason
- `internal/bundle` TestApprovalIsASecondPersonAgainstTheSealedDigest — plan, submission and approval are journaled; the approver is a second person
```

- [ ] **Step 5: Update `AGENTS.md`**

1. In the status line, append: `Phase 20 (Governance-as-Code, ADR-026) is complete; Phase 21 extends it to identity, policy and budgets.`
2. Add a rule bullet after the Fleet operations bullet:

```markdown
- Governance-as-Code never adds authority (ADR-026). A change set's steps are `registry.Tx` writes, so the registry triggers decide each one; never write registry tables for a bundle any other way. PostgreSQL computes every change-set digest; a stale change set runs nothing. Submit and approve are different principals, each stage one transaction. Nothing is deleted: prune only retires a non-ACTIVE version or revokes a contract. A bundle never activates a version beside an ACTIVE one (release, ADR-018), never undoes containment, never declares an MCP tool, and never carries a secret value. Drift is read-only.
```

3. In the Layout block, add after `internal/fleet`:

```text
internal/bundle      Governance-as-Code: desired documents, snapshot planner, two-person change sets, drift (ADR-026)
```

- [ ] **Step 6: Run the full suite**

Run:

```bash
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go vet ./... && go test -race -count=1 ./...
```

Expected: PASS, including `test/invariants`, which checks that every test named in `docs/INVARIANTS.md` exists.

- [ ] **Step 7: Commit**

```bash
git add docs/adr/ADR-026-governance-as-code.md docs/adr/README.md docs/MASTER_PLAN.md docs/INVARIANTS.md AGENTS.md
git commit -m "docs: ADR-026 Governance-as-Code; Phase 20 delivered, later phases renumbered"
```

Then stop and report to the user (MASTER_PLAN §107). Do not start Phase 21.
