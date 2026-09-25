# Phase 21 Governance-as-Code (identity, policy, budgets, prices) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend the Phase 20 bundle engine so one bundle can also declare principals, groups, memberships, role grants, the tenant policy, budget accounts with hard and soft limits, and model prices, applied through the same two-person change sets.

**Architecture:** `internal/bundle` grows one planner file per domain and one executor file; each step writes through a `Tx` that holds the domain's SQL (`registry.Tx`, `governance.Tx`, `budget.Tx`, `finops.Tx`), shared with the API services, so the existing triggers decide every step. Migration 00021 adds no table: it widens the change-set CHECKs, teaches `change_set_ref_row` the new kinds, allows one policy owner per tenant and enforces a two-admin floor at the stage's commit.

**Tech Stack:** Go 1.27, pgx v5, PostgreSQL 16 (goose migrations), `go.yaml.in/yaml/v3` in eacpctl.

**Spec:** `docs/superpowers/specs/2026-09-25-governance-as-code-phase-21-design.md` (builds on `docs/superpowers/specs/2026-09-25-governance-as-code-design.md` and ADR-026 Rev 1.0).

## Global Constraints

- A bundle can do only what the same admin could do through the API; each step passes its own trigger (spec §10.1).
- Nothing is disabled, deleted or backdated; prune only revokes grants and removes memberships (spec §10.5).
- No change set leaves fewer than two approved admins on enabled human principals when it revoked one (spec §10.3).
- Budget counters (`allocated`, `reserved`, `committed`) never enter a digest (spec §6).
- Credentials never appear in a bundle; decoding stays strict (unknown fields refused).
- Tests first; run with `-race` and `EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"` (without it DB tests skip, which is not a pass).
- Never weaken a test or a fail-closed rule. Commit as the user only, with no Co-Authored-By trailer.
- Addresses match `kind.slug[.slug]` (00020 CHECK) and the seal takes a ref's kind from the address prefix, so the new prefixes are the kinds: `principal`, `group`, `member`, `role`, `policy`, `budget`, `price`. The tenant policy's address is `policy.tenant`; prices are keyed by a slug name because model names contain dots (spec deviations, recorded as rulings when executed).

## Review Focus

1. **Equal numbers written differently** (`2.5`, `2.50`, `1000.000`) must plan nothing for a limit or a price. Tests: `TestBudgetLimitsAreComparedByValueAndLoweredChildrenFirst`, `TestAPriceIsAddedWhenItDiffersFromTheOneInEffect` (Task 5), `TestAPriceIsAddedForwardOnly` (Task 6).
2. **A prune that removes the second-to-last admin**, planned or raced by an API revoke, must never commit. Tests: `TestTheAdminFloorBlocksAPlanThatLeavesOneAdmin` (Task 4), `TestAStageThatRevokedAnAdminCannotLeaveFewerThanTwo` (Task 2), `TestAConcurrentRevokeCannotBreakTheAdminFloor` (Task 6).
3. **A reformatted policy** (key order, whitespace) must plan nothing. Tests: `TestAPolicyIsCreatedThenActivatedAndComparedCanonically` (Task 5), `TestAPolicyIsCreatedByOneAdminAndActivatedByAnother` (Task 6).
4. **Action traffic** moving budget counters must never make a plan stale. Test: `TestABudgetRefIgnoresItsCounters` (Task 2).
5. **A policy file outside the bundle directory** must be refused, not read. Test: `TestBundleGovernanceSectionsAndPolicyFile` (Task 7).
6. **A pending proposal** left by a rejected change set must block with a message that names what to decide. Test: `TestAPendingGrantBlocks` (Task 4) checks the detail names the grant.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/registry/tx.go` (modify) | identity writes on `registry.Tx` |
| `internal/registry/principals.go` (modify) | service methods call `Tx` |
| `internal/governance/tx.go` (create), `store.go` (modify) | policy writes |
| `internal/budget/tx.go` (create), `budget.go` (modify) | account and limit writes |
| `internal/finops/tx.go` (create), `finops.go` (modify) | price and soft-limit writes |
| `migrations/00021_governance_as_code_identity.sql` (create) | kinds, ref rows, policy owner, admin floor |
| `internal/bundle/resources.go` (create) | Phase 21 document types, validation, amounts |
| `internal/bundle/document.go` (modify) | document fields, imports, declared |
| `internal/bundle/state.go` (modify), `state_governance.go` (create) | state of people, groups, policy, budgets, prices |
| `internal/bundle/plan.go` (modify) | payload fields, planner order, owner by address, admin floor |
| `internal/bundle/plan_identity.go` (create) | principals, roles, groups, members |
| `internal/bundle/plan_policy.go` (create) | tenant policy |
| `internal/bundle/plan_budget.go` (create) | budgets and prices |
| `internal/bundle/execute.go` (modify), `execute_governance.go` (create) | new step writes, managed kinds |
| `internal/bundle/service.go` (modify) | admin-floor error mapping |
| `cmd/eacpctl/bundle.go` (modify) | new sections, policy file |
| `test/demo/slice_c_test.go`, docs (modify) | demo step, ADR-026 Rev 1.1, INVARIANTS, AGENTS, MASTER_PLAN |

---

### Task 1: Shared transactions for identity, policy, budgets and prices

**Files:**
- Modify: `internal/registry/tx.go`, `internal/registry/principals.go`
- Create: `internal/governance/tx.go`, `internal/budget/tx.go`, `internal/finops/tx.go`
- Modify: `internal/governance/store.go`, `internal/budget/budget.go`, `internal/finops/finops.go`
- Test: `internal/registry/tx_identity_test.go`, `internal/governance/tx_test.go`, `internal/budget/tx_test.go`, `internal/finops/tx_test.go`

**Interfaces:**
- Produces: `registry.Tx.{CreatePrincipal(ctx, NewPrincipal) (Principal, error), ProposeRole(ctx, principalID, role) (uuid.UUID, error), ApproveRole(ctx, grantID) error, RevokeRole(ctx, grantID, reason) error, CreateGroup(ctx, name, displayName string, weight int) (uuid.UUID, error), AddMember(ctx, groupID, principalID) (uuid.UUID, error), RemoveMember(ctx, membershipID, reason) error}`; `governance.Tx{pgx.Tx}.{CreatePolicy(ctx, json.RawMessage) (Policy, error), ActivatePolicy(ctx, id, reason) error}`; `budget.Tx{pgx.Tx}.{CreateAccount(ctx, NewAccount) (uuid.UUID, error), ChangeLimit(ctx, account, limit, reason string) (LimitChange, error), DecideLimitChange(ctx, change, apply bool, reason string) (LimitChange, error)}`; `finops.Tx{pgx.Tx}.{AddPrice(ctx, NewPrice) (Price, error), SetSoftLimit(ctx, account, *string, reason) error}`.

- [ ] **Step 1: Write the failing tests**

`internal/registry/tx_identity_test.go`:
```go
package registry_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

// txAs runs fn as actor in one transaction of the fixture tenant.
func txAs(t *testing.T, f *registrytest.Fixture, actor string, fn func(ctx context.Context, rtx registry.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
			return err
		}
		return fn(ctx, registry.Tx{Tx: tx})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTxIdentityWritesRunInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	var dana, grant, group, member uuid.UUID
	txAs(t, f, "alice", func(ctx context.Context, rtx registry.Tx) error {
		p, err := rtx.CreatePrincipal(ctx, registry.NewPrincipal{Kind: "human", Name: "dana",
			Subject: " Dana@Example.com ", DisplayName: "Dana"})
		if err != nil {
			return err
		}
		dana = p.ID
		if grant, err = rtx.ProposeRole(ctx, dana, "auditor"); err != nil {
			return err
		}
		if group, err = rtx.CreateGroup(ctx, "auditors", "Auditors", 3); err != nil {
			return err
		}
		member, err = rtx.AddMember(ctx, group, dana)
		return err
	})
	txAs(t, f, "bob", func(ctx context.Context, rtx registry.Tx) error {
		if err := rtx.ApproveRole(ctx, grant); err != nil {
			return err
		}
		if err := rtx.RemoveMember(ctx, member, "moved"); err != nil {
			return err
		}
		return rtx.RevokeRole(ctx, grant, "moved")
	})
	var n int
	if err := storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT count(*) FROM eacp.principals p
			JOIN eacp.role_grants g ON g.principal_id = p.id AND g.approved_by = $2 AND g.revoked_at IS NOT NULL
			JOIN eacp.group_memberships m ON m.principal_id = p.id AND m.removed_at IS NOT NULL
			JOIN eacp.groups gr ON gr.id = m.group_id AND gr.schedule_weight = 3
			WHERE p.id = $1 AND p.subject = 'dana@example.com'`, dana, f.P["bob"]).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("identity writes = %d, %v", n, err)
	}
}
```

`internal/governance/tx_test.go`:
```go
package governance_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestTxCreatesAndActivatesInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	in := func(actor string, fn func(gtx governance.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
				return err
			}
			return fn(governance.Tx{Tx: tx})
		})
	}
	err := in("alice", func(gtx governance.Tx) error {
		_, err := gtx.CreatePolicy(ctx, json.RawMessage(`{"rules":[]}`))
		return err
	})
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("invalid policy: %v", err)
	}
	var p governance.Policy
	if err := in("alice", func(gtx governance.Tx) error {
		var err error
		p, err = gtx.CreatePolicy(ctx,
			json.RawMessage(`{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := in("bob", func(gtx governance.Tx) error { return gtx.ActivatePolicy(ctx, p.ID, "reviewed") }); err != nil {
		t.Fatal(err)
	}
	cur, err := governance.NewStore(f.App).CurrentPolicy(ctx, f.Tenant)
	if err != nil || cur.ID != p.ID || p.Version != 1 {
		t.Fatalf("current = %+v, %v (created %+v)", cur, err, p)
	}
}
```

`internal/budget/tx_test.go`:
```go
package budget_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/budget"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestTxChangesLimitsInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	in := func(actor string, fn func(btx budget.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P[actor]); err != nil {
				return err
			}
			return fn(budget.Tx{Tx: tx})
		})
	}
	var acct uuid.UUID
	var raise budget.LimitChange
	if err := in("alice", func(btx budget.Tx) error {
		var err error
		if acct, err = btx.CreateAccount(ctx, budget.NewAccount{Name: "ops", Unit: "USD"}); err != nil {
			return err
		}
		raise, err = btx.ChangeLimit(ctx, acct, "100", "fund")
		return err
	}); err != nil || raise.State != "PROPOSED" {
		t.Fatalf("raise = %+v, %v", raise, err)
	}
	if err := in("alice", func(btx budget.Tx) error {
		_, err := btx.ChangeLimit(ctx, acct, "1e3", "bad")
		return err
	}); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("malformed limit: %v", err)
	}
	var applied, lowered budget.LimitChange
	if err := in("bob", func(btx budget.Tx) error {
		var err error
		applied, err = btx.DecideLimitChange(ctx, raise.ID, true, "ok")
		return err
	}); err != nil || applied.State != "APPLIED" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	if err := in("alice", func(btx budget.Tx) error {
		var err error
		lowered, err = btx.ChangeLimit(ctx, acct, "40", "trim")
		return err
	}); err != nil || lowered.State != "APPLIED" {
		t.Fatalf("decrease = %+v, %v", lowered, err)
	}
	got, err := budget.New(f.App).Get(ctx, registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}, acct)
	if err != nil || got.HardLimit != "40" {
		t.Fatalf("account = %+v, %v", got, err)
	}
}
```

`internal/finops/tx_test.go`:
```go
package finops_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/finops"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestTxAddsPricesAndSoftLimitsInTheCallersTransaction(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	acct := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'ops', 'USD') RETURNING id`)
	in := func(fn func(ftx finops.Tx) error) error {
		return storage.InTenantTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
			if err := storage.SetActor(ctx, tx, f.P["alice"]); err != nil {
				return err
			}
			return fn(finops.Tx{Tx: tx})
		})
	}
	if err := in(func(ftx finops.Tx) error {
		_, err := ftx.AddPrice(ctx, finops.NewPrice{Provider: "openai", Model: "gpt-4.1", Unit: "USD",
			InputPerMTok: "-1", OutputPerMTok: "10", Reason: "card"})
		return err
	}); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("negative price: %v", err)
	}
	var p finops.Price
	limit := "150"
	if err := in(func(ftx finops.Tx) error {
		var err error
		if p, err = ftx.AddPrice(ctx, finops.NewPrice{Provider: "openai", Model: "gpt-4.1", Unit: "USD",
			InputPerMTok: "2.5", OutputPerMTok: "10", Reason: "card"}); err != nil {
			return err
		}
		return ftx.SetSoftLimit(ctx, acct, &limit, "watch")
	}); err != nil || p.InputPerMTok != "2.5" {
		t.Fatalf("price = %+v, %v", p, err)
	}
	alice := registry.Actor{TenantID: f.Tenant, PrincipalID: f.P["alice"]}
	limits, err := finops.New(f.App).SoftLimits(ctx, alice)
	if err != nil || len(limits) != 1 || limits[0].MonthlyLimit == nil || *limits[0].MonthlyLimit != "150" {
		t.Fatalf("soft limits = %+v, %v", limits, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/registry/ ./internal/governance/ ./internal/budget/ ./internal/finops/ -run 'TestTx'`
Expected: FAIL to compile: `rtx.CreatePrincipal undefined`, `undefined: governance.Tx`, `undefined: budget.Tx`, `undefined: finops.Tx`.

- [ ] **Step 3: Implement the transactions**

Append to `internal/registry/tx.go` (add `"strings"` to its imports):
```go
// CreatePrincipal inserts a principal; it holds no roles until two admins
// grant one. A human's subject is canonicalised to lower case.
func (t Tx) CreatePrincipal(ctx context.Context, p NewPrincipal) (Principal, error) {
	out := Principal{Kind: p.Kind, Name: p.Name, DisplayName: p.DisplayName,
		Subject: strings.ToLower(strings.TrimSpace(p.Subject))}
	err := t.QueryRow(ctx, `
		INSERT INTO eacp.principals (tenant_id, kind, name, subject, display_name)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		out.Kind, out.Name, nullStr(out.Subject), out.DisplayName).Scan(&out.ID)
	return out, err
}

// ProposeRole proposes granting role to a principal; a second admin approves.
func (t Tx) ProposeRole(ctx context.Context, principalID uuid.UUID, role string) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, principalID, role).Scan(&id)
	return id, err
}

// ApproveRole makes a proposed grant effective.
func (t Tx) ApproveRole(ctx context.Context, grantID uuid.UUID) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grantID)
}

// RevokeRole permanently revokes a grant.
func (t Tx) RevokeRole(ctx context.Context, grantID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = $2 WHERE id = $1`,
		grantID, reason)
}

// CreateGroup inserts a group with its scheduler claim quantum.
func (t Tx) CreateGroup(ctx context.Context, name, displayName string, weight int) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
		VALUES (eacp.current_tenant_id(), $1, $2, $3) RETURNING id`, name, displayName, weight).Scan(&id)
	return id, err
}

// AddMember adds a principal to a group and returns the membership id.
func (t Tx) AddMember(ctx context.Context, groupID, principalID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, groupID, principalID).Scan(&id)
	return id, err
}

// RemoveMember closes a membership.
func (t Tx) RemoveMember(ctx context.Context, membershipID uuid.UUID, reason string) error {
	return execOne(ctx, t.Tx, `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = $2 WHERE id = $1`,
		membershipID, reason)
}
```

In `internal/registry/principals.go`, replace the bodies of `CreatePrincipal`, `ProposeRole`, `ApproveRole`, `RevokeRole`, `CreateGroupWeighted`, `AddMember` and `RemoveMember` (keep their doc comments; `CreateGroup` already calls `CreateGroupWeighted`; drop the now-unused `strings` import if nothing else uses it):
```go
func (s *Service) CreatePrincipal(ctx context.Context, a Actor, p NewPrincipal) (Principal, error) {
	var out Principal
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		out, err = Tx{tx}.CreatePrincipal(ctx, p)
		return err
	})
	return out, err
}

func (s *Service) ProposeRole(ctx context.Context, a Actor, principalID uuid.UUID, role string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.ProposeRole(ctx, principalID, role)
		return err
	})
	return id, err
}

func (s *Service) ApproveRole(ctx context.Context, a Actor, grantID uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.ApproveRole(ctx, grantID) })
}

func (s *Service) RevokeRole(ctx context.Context, a Actor, grantID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.RevokeRole(ctx, grantID, reason) })
}

func (s *Service) CreateGroupWeighted(ctx context.Context, a Actor, name, displayName string, weight int) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.CreateGroup(ctx, name, displayName, weight)
		return err
	})
	return id, err
}

func (s *Service) AddMember(ctx context.Context, a Actor, groupID, principalID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.AddMember(ctx, groupID, principalID)
		return err
	})
	return id, err
}

func (s *Service) RemoveMember(ctx context.Context, a Actor, membershipID uuid.UUID, reason string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.RemoveMember(ctx, membershipID, reason) })
}
```

`internal/governance/tx.go`:
```go
package governance

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Tx makes policy writes in a caller's transaction, which must already have
// its tenant and actor set. Store uses it, and a bundle change set (ADR-026)
// uses it, so the policy triggers decide both the same way. Errors are raw
// database errors, except content that fails validation.
type Tx struct{ pgx.Tx }

// CreatePolicy validates the exact local bundle, then stores an immutable
// version. PostgreSQL assigns the version under the tenant pointer lock.
func (t Tx) CreatePolicy(ctx context.Context, content json.RawMessage) (Policy, error) {
	out := Policy{Content: content}
	if err := ValidatePolicy(content); err != nil {
		return out, &registry.Error{Kind: registry.ErrInvalid, Msg: err.Error()}
	}
	err := t.QueryRow(ctx, `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id, version`, content).Scan(&out.ID, &out.Version)
	return out, err
}

// ActivatePolicy points the tenant at policy id. The pointer trigger needs an
// admin who did not author it and a higher version.
func (t Tx) ActivatePolicy(ctx context.Context, id uuid.UUID, reason string) error {
	tag, err := t.Exec(ctx, `UPDATE eacp.tenant_policy_pointer
		SET current_bundle_id = $1, activation_reason = $2
		WHERE tenant_id = eacp.current_tenant_id()`, id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
```

In `internal/governance/store.go`, `CreatePolicy` and `ActivatePolicy` keep their actor checks and transaction, and write through `Tx` (the validation moves into `Tx.CreatePolicy`):
```go
func (s *Store) CreatePolicy(ctx context.Context, actor registry.Actor, content json.RawMessage) (Policy, error) {
	var out Policy
	if actor.TenantID == uuid.Nil || actor.PrincipalID == uuid.Nil {
		return out, &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	err := storage.InTenantTx(ctx, s.pool, actor.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, actor.PrincipalID); err != nil {
			return err
		}
		var err error
		out, err = Tx{tx}.CreatePolicy(ctx, content)
		return err
	})
	out.Content = content
	return out, storeErr(err)
}

func (s *Store) ActivatePolicy(ctx context.Context, actor registry.Actor, id uuid.UUID, reason string) error {
	if actor.TenantID == uuid.Nil || actor.PrincipalID == uuid.Nil {
		return &registry.Error{Kind: registry.ErrForbidden, Msg: "no actor"}
	}
	err := storage.InTenantTx(ctx, s.pool, actor.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetActor(ctx, tx, actor.PrincipalID); err != nil {
			return err
		}
		return Tx{tx}.ActivatePolicy(ctx, id, reason)
	})
	return storeErr(err)
}
```

`internal/budget/tx.go`:
```go
package budget

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/registry"
)

// Tx makes budget writes in a caller's transaction, which must already have
// its tenant and actor set. Service uses it, and a bundle change set
// (ADR-026) uses it, so the budget triggers decide both the same way. Errors
// are raw database errors, except an amount that fails validation.
type Tx struct{ pgx.Tx }

// CreateAccount inserts an empty account; its limit starts at zero.
func (t Tx) CreateAccount(ctx context.Context, n NewAccount) (uuid.UUID, error) {
	var id uuid.UUID
	err := t.QueryRow(ctx, `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id, agent_id)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4) RETURNING id`,
		n.Name, n.Unit, n.ParentID, n.AgentID).Scan(&id)
	return id, err
}

// ChangeLimit records a limit change: a decrease applies at once, an
// increase stays PROPOSED until a different admin applies it.
func (t Tx) ChangeLimit(ctx context.Context, account uuid.UUID, limit, reason string) (LimitChange, error) {
	if !amountPattern.MatchString(limit) {
		return LimitChange{}, &registry.Error{Kind: registry.ErrInvalid,
			Msg: "limit must be a non-negative decimal below 10^15 with at most 6 decimals"}
	}
	return scanChange(t.QueryRow(ctx, `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2::numeric, $3) RETURNING `+changeColumns, account, limit, reason))
}

// DecideLimitChange applies or rejects an open increase.
func (t Tx) DecideLimitChange(ctx context.Context, change uuid.UUID, apply bool, reason string) (LimitChange, error) {
	state := "REJECTED"
	if apply {
		state = "APPLIED"
	}
	return scanChange(t.QueryRow(ctx, `UPDATE eacp.budget_limit_changes SET state = $2, decision_reason = $3
		WHERE id = $1 RETURNING `+changeColumns, change, state, reason))
}
```

In `internal/budget/budget.go`, the three writes go through `Tx`:
```go
func (s *Service) CreateAccount(ctx context.Context, a registry.Actor, n NewAccount) (Account, error) {
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		id, err = Tx{tx}.CreateAccount(ctx, n)
		return err
	})
	if err != nil {
		return Account{}, err
	}
	return s.Get(ctx, a, id)
}

func (s *Service) ChangeLimit(ctx context.Context, a registry.Actor, account uuid.UUID, limit, reason string) (LimitChange, error) {
	var c LimitChange
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.ChangeLimit(ctx, account, limit, reason)
		return err
	})
	return c, err
}

func (s *Service) DecideLimitChange(ctx context.Context, a registry.Actor, change uuid.UUID, apply bool, reason string) (LimitChange, error) {
	var c LimitChange
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		c, err = Tx{tx}.DecideLimitChange(ctx, change, apply, reason)
		return err
	})
	return c, err
}
```

`internal/finops/tx.go`:
```go
package finops

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Tx makes rate-card and soft-limit writes in a caller's transaction, which
// must already have its tenant and actor set. Service uses it, and a bundle
// change set (ADR-026) uses it, so the FinOps triggers decide both the same
// way. Errors are raw database errors, except an amount that fails
// validation.
type Tx struct{ pgx.Tx }

// AddPrice adds a price, effective now unless a later effective_from is
// given; the price trigger refuses a backdated one.
func (t Tx) AddPrice(ctx context.Context, n NewPrice) (Price, error) {
	for _, v := range []*string{&n.InputPerMTok, n.CachedPerMTok, &n.OutputPerMTok} {
		if v != nil && !amountPattern.MatchString(*v) {
			return Price{}, invalid("prices must be non-negative decimals below 10^15 with at most 6 decimals")
		}
	}
	return scanPrice(t.QueryRow(ctx, `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit,
		input_per_mtok, cached_input_per_mtok, output_per_mtok, effective_from, reason)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4::numeric, $5::numeric, $6::numeric, COALESCE($7, now()), $8)
		RETURNING `+priceColumns,
		n.Provider, n.Model, n.Unit, n.InputPerMTok, n.CachedPerMTok, n.OutputPerMTok, n.EffectiveFrom, n.Reason))
}

// SetSoftLimit sets or clears (limit nil) an account's soft limit.
func (t Tx) SetSoftLimit(ctx context.Context, account uuid.UUID, limit *string, reason string) error {
	if limit != nil && !amountPattern.MatchString(*limit) {
		return invalid("monthly_limit must be a positive decimal with at most 6 decimals")
	}
	_, err := t.Exec(ctx, `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, $2::numeric, $3)
		ON CONFLICT (tenant_id, account_id) DO UPDATE SET monthly_limit = EXCLUDED.monthly_limit,
		reason = EXCLUDED.reason`, account, limit, reason)
	return err
}
```

In `internal/finops/finops.go`, `AddPrice` and `SetSoftLimit` go through `Tx`:
```go
func (s *Service) AddPrice(ctx context.Context, a registry.Actor, n NewPrice) (Price, error) {
	var p Price
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		p, err = Tx{tx}.AddPrice(ctx, n)
		return err
	})
	return p, err
}
```
and in `SetSoftLimit`, drop its own validation and replace the `tx.Exec` block with
```go
	err := s.change(ctx, a, func(tx pgx.Tx) error { return Tx{tx}.SetSoftLimit(ctx, account, limit, reason) })
```
keeping the read of the stored soft limit that follows.

- [ ] **Step 4: Run the tests and the domain suites**

Run: `go vet ./internal/registry/ ./internal/governance/ ./internal/budget/ ./internal/finops/ && go test -race -count=1 ./internal/registry/ ./internal/governance/ ./internal/budget/ ./internal/finops/ ./internal/api/`
Expected: PASS (the new `TestTx…` tests and every existing test of the four packages and the API).

- [ ] **Step 5: Commit**

```bash
git add internal/registry internal/governance internal/budget internal/finops
git commit -m "refactor: identity, policy, budget and price writes on shared Tx types"
```

---

### Task 2: Migration 00021 — kinds, ref rows, one policy owner, the admin floor

**Files:**
- Create: `migrations/00021_governance_as_code_identity.sql`
- Test: `internal/bundle/schema_phase21_test.go`

**Interfaces:**
- Consumes: 00020 tables and functions (`change_set_ref_row`, `bundle_resources_guard`, `change_sets_commit`).
- Produces: step addresses and ref kinds `principal|group|member|role|policy|budget|price`; op `set`; `bundle_resources` kinds `principal|group|policy|budget|price`; the commit error `SQLSTATE 23514, HINT 'admin_floor'`.

- [ ] **Step 1: Write the failing tests**

`internal/bundle/schema_phase21_test.go`:
```go
package bundle_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"eacp/internal/registry/registrytest"
)

const allowPolicy = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`

func TestPhase21StepKindsAndTheSetOp(t *testing.T) {
	f := registrytest.New(t)
	steps := []rawStep{{"principal.dana", "create", "submit"}, {"group.ops", "create", "submit"},
		{"member.ops.dana", "create", "submit"}, {"role.dana.auditor", "propose", "submit"},
		{"policy.tenant", "create", "submit"}, {"budget.ops", "set", "submit"}, {"price.gpt", "create", "submit"},
		{"role.dana.auditor", "activate", "approve"}}
	_, err := plan(f, "alice", "people", steps, []rawRef{{"principal", f.P["carol"]}, {"policy", f.Tenant},
		{"member", uuid.New()}, {"role", uuid.New()}, {"budget", uuid.New()}, {"price", uuid.New()}})
	ok(t, err)
	_, err = plan(f, "alice", "bad-kind", []rawStep{{"secret.x", "create", "submit"}}, nil)
	wantState(t, err, sqlCheck)
	_, err = plan(f, "alice", "bad-op", []rawStep{{"budget.x", "delete", "submit"}}, nil)
	wantState(t, err, sqlCheck)
	_, err = plan(f, "alice", "bad-ref", []rawStep{{"budget.x", "set", "submit"}}, []rawRef{{"secret", uuid.New()}})
	wantState(t, err, sqlCheck)
}

func TestOnlyOneBundleOwnsTheTenantPolicy(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, f.DB.AdminDSN)
	ok(t, err)
	defer admin.Close(ctx)
	// Triggers and foreign keys off: only the index is under test.
	_, err = admin.Exec(ctx, `SET session_replication_role = replica`)
	ok(t, err)
	insert := func() error {
		_, err := admin.Exec(ctx, `INSERT INTO eacp.bundle_resources
			(tenant_id, bundle_id, address, kind, object_id, change_set_id, managed_at)
			VALUES ($1, $2, 'policy.tenant', 'policy', $3, $4, now())`, f.Tenant, uuid.New(), uuid.New(), uuid.New())
		return err
	}
	ok(t, insert())
	wantState(t, insert(), sqlUnique)
}

func TestAStageThatRevokedAnAdminCannotLeaveFewerThanTwo(t *testing.T) {
	f := registrytest.New(t)
	steps := []rawStep{{"principal.alice", "import", "submit"}, {"role.alice.admin", "revoke", "approve"}}
	id, err := plan(f, "alice", "admins", steps, nil)
	ok(t, err)
	ok(t, inTx(f, "alice", func(ctx context.Context, tx pgx.Tx) error {
		if err := setState(ctx, tx, id, "SUBMITTED"); err != nil {
			return err
		}
		if err := runSteps(ctx, tx, id, 1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT eacp.change_set_seal($1)`, id)
		return err
	}))
	approve := func() error {
		return inTx(f, "bob", func(ctx context.Context, tx pgx.Tx) error {
			if err := setState(ctx, tx, id, "APPLIED"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'prune'
				WHERE principal_id = $1 AND role = 'admin' AND revoked_at IS NULL`, f.P["alice"]); err != nil {
				return err
			}
			return runSteps(ctx, tx, id, 2)
		})
	}
	err = approve()
	wantState(t, err, sqlCheck)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Hint != "admin_floor" {
		t.Fatalf("admin floor error = %v", err)
	}
	// With a third admin, the same stage commits and two remain.
	grant := f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, 'admin') RETURNING id`, f.P["carol"])
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, grant))
	ok(t, approve())
}

func refRow(t *testing.T, f *registrytest.Fixture, kind string, id uuid.UUID) string {
	t.Helper()
	var s string
	ok(t, inTx(f, "alice", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.change_set_ref_row($1, $2)`, kind, id).Scan(&s)
	}))
	return s
}

func TestABudgetRefIgnoresItsCounters(t *testing.T) {
	f := registrytest.New(t)
	raise := func(acct uuid.UUID, limit string) {
		change := f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
			VALUES (eacp.current_tenant_id(), $1, $2::numeric, 'fund') RETURNING id`, acct, limit)
		ok(t, f.Exec("bob", `UPDATE eacp.budget_limit_changes SET state = 'APPLIED', decision_reason = 'ok'
			WHERE id = $1`, change))
	}
	root := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'root', 'USD') RETURNING id`)
	raise(root, "100")
	before := refRow(t, f, "budget", root)
	team := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit, parent_id)
		VALUES (eacp.current_tenant_id(), 'team', 'USD', $1) RETURNING id`, root)
	raise(team, "40") // escrowed from root: root's allocated counter moves
	if after := refRow(t, f, "budget", root); after != before {
		t.Fatalf("a counter changed the ref row:\n%s\n%s", before, after)
	}
	f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 50, 'trim') RETURNING id`, root)
	if refRow(t, f, "budget", root) == before {
		t.Fatal("a limit change must change the ref row")
	}
}

func TestAPolicyRefFollowsTheTenantPointer(t *testing.T) {
	f := registrytest.New(t)
	if refRow(t, f, "policy", uuid.New()) != "absent" {
		t.Fatal("an unknown policy is absent")
	}
	before := refRow(t, f, "policy", f.Tenant)
	if before == "absent" {
		t.Fatal("the tenant's policy ref exists before any policy")
	}
	pol := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, allowPolicy)
	ok(t, f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1, activation_reason = 'go'
		WHERE tenant_id = eacp.current_tenant_id()`, pol))
	if refRow(t, f, "policy", f.Tenant) == before {
		t.Fatal("activating a policy must change the tenant's policy ref")
	}
	if refRow(t, f, "policy", pol) == "absent" {
		t.Fatal("a policy version is present")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/bundle/ -run 'Phase21|OnlyOneBundleOwns|StageThatRevoked|BudgetRef|PolicyRef'`
Expected: FAIL: the CHECKs refuse `principal.dana` (`23514`), the second policy row is accepted, the stage commits, `change_set_ref_row` raises `unknown ref kind budget`.

- [ ] **Step 3: Confirm the constraint names**

Run: `docker compose exec -T postgres psql -U postgres -d postgres -Atc "SELECT conname FROM pg_constraint WHERE conrelid IN ('eacp.change_set_steps'::regclass, 'eacp.change_set_refs'::regclass, 'eacp.bundle_resources'::regclass) AND contype = 'c' ORDER BY 1"` against a migrated test database (any `eacp_test_*` database a test left, or run `go test -run TestPhase21StepKindsAndTheSetOp ./internal/bundle/` first and read it from its log).
Expected: `bundle_resources_kind_check`, `change_set_refs_kind_check`, `change_set_steps_address_check`, `change_set_steps_check`, `change_set_steps_op_check`, `change_set_steps_payload_check`, `change_set_steps_ordinal_check`, `change_set_steps_stage_check`. If a name differs, use the real name in the migration.

- [ ] **Step 4: Write the migration**

`migrations/00021_governance_as_code_identity.sql` (Up below; the Down section is generated in Step 5):
```sql
-- Phase 21 (ADR-026 Rev 1.1): Governance-as-Code for identity, the tenant
-- policy, budgets and prices. No new table: change sets learn the new step
-- and ref kinds, one bundle may own the tenant policy, the digest learns the
-- new objects (never the budget counters, which move with every action), and
-- a stage that revoked an admin grant cannot commit below two admins.
-- +goose Up

ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_address_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_address_check CHECK (address ~
    '^(connector|tool|contract|agent|version|allowlist|principal|group|member|role|policy|budget|price)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$');
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_op_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_op_check
    CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import', 'set'));
ALTER TABLE eacp.change_set_refs DROP CONSTRAINT change_set_refs_kind_check;
ALTER TABLE eacp.change_set_refs ADD CONSTRAINT change_set_refs_kind_check CHECK (kind IN ('connector', 'tool',
    'contract', 'agent', 'version', 'allowlist', 'principal', 'group', 'member', 'role', 'policy', 'budget', 'price'));
ALTER TABLE eacp.bundle_resources DROP CONSTRAINT bundle_resources_kind_check;
ALTER TABLE eacp.bundle_resources ADD CONSTRAINT bundle_resources_kind_check CHECK (kind IN ('connector', 'tool',
    'agent', 'version', 'principal', 'group', 'policy', 'budget', 'price'));
-- One bundle owns the tenant policy.
CREATE UNIQUE INDEX bundle_resources_one_policy ON eacp.bundle_resources (tenant_id) WHERE kind = 'policy';

-- +goose StatementBegin
-- The part of a registry object a plan depends on, as text, or 'absent'.
-- Under RLS it sees only the current tenant.
CREATE OR REPLACE FUNCTION eacp.change_set_ref_row(p_kind text, p_id uuid) RETURNS text
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
        SELECT jsonb_build_array(p.id, p.name, p.kind, p.subject, p.display_name, p.disabled_at IS NULL,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(g.id, g.role, g.approved_at IS NOT NULL) ORDER BY g.role),
                                '[]'::jsonb)
                  FROM eacp.role_grants g
                 WHERE g.tenant_id = p.tenant_id AND g.principal_id = p.id AND g.revoked_at IS NULL))::text
          INTO r FROM eacp.principals p WHERE p.id = p_id;
    WHEN 'group' THEN
        SELECT jsonb_build_array(g.id, g.name, g.display_name, g.schedule_weight,
               (SELECT COALESCE(jsonb_agg(jsonb_build_array(m.id, m.principal_id) ORDER BY m.id), '[]'::jsonb)
                  FROM eacp.group_memberships m
                 WHERE m.tenant_id = g.tenant_id AND m.group_id = g.id AND m.removed_at IS NULL))::text
          INTO r FROM eacp.groups g WHERE g.id = p_id;
    WHEN 'member' THEN
        SELECT jsonb_build_array(m.id, m.group_id, m.principal_id, m.removed_at IS NULL)::text
          INTO r FROM eacp.group_memberships m WHERE m.id = p_id;
    WHEN 'role' THEN
        SELECT jsonb_build_array(g.id, g.principal_id, g.role, g.approved_at IS NOT NULL, g.revoked_at IS NULL)::text
          INTO r FROM eacp.role_grants g WHERE g.id = p_id;
    WHEN 'policy' THEN
        -- A policy version, or the tenant itself when it has none: either way
        -- the pointer is part of the row, so every activation moves it.
        SELECT jsonb_build_array(p_id, pb.version, pb.revoked_at IS NULL, ptr.current_bundle_id)::text
          INTO r FROM eacp.tenant_policy_pointer ptr
          LEFT JOIN eacp.policy_bundles pb ON pb.tenant_id = ptr.tenant_id AND pb.id = p_id
         WHERE ptr.tenant_id = eacp.current_tenant_id() AND (pb.id IS NOT NULL OR p_id = ptr.tenant_id);
    WHEN 'budget' THEN
        -- Never allocated, reserved or committed: they move with every action.
        SELECT jsonb_build_array(b.id, b.name, b.unit, b.parent_id, b.agent_id, trim_scale(b.hard_limit)::text,
               (SELECT trim_scale(l.monthly_limit)::text FROM eacp.budget_soft_limits l
                 WHERE l.tenant_id = b.tenant_id AND l.account_id = b.id),
               (SELECT c.id FROM eacp.budget_limit_changes c
                 WHERE c.tenant_id = b.tenant_id AND c.account_id = b.id AND c.state = 'PROPOSED'
                 ORDER BY c.proposed_at DESC LIMIT 1))::text
          INTO r FROM eacp.budget_accounts b WHERE b.id = p_id;
    WHEN 'price' THEN
        SELECT jsonb_build_array(mp.id, mp.provider, mp.model,
               (SELECT cur.id FROM eacp.model_prices cur
                 WHERE cur.tenant_id = mp.tenant_id AND cur.provider = mp.provider AND cur.model = mp.model
                   AND cur.effective_from <= now()
                 ORDER BY cur.effective_from DESC LIMIT 1))::text
          INTO r FROM eacp.model_prices mp WHERE mp.id = p_id;
    ELSE
        RAISE EXCEPTION 'unknown ref kind %', p_kind USING ERRCODE = '23514';
    END CASE;
    RETURN COALESCE(r, 'absent');
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- An address manages an object only when the executing change set of its
-- bundle created or imported that object in this transaction.
CREATE OR REPLACE FUNCTION eacp.bundle_resources_guard() RETURNS trigger
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
    IF NEW.kind NOT IN ('connector', 'tool', 'agent', 'version', 'principal', 'group', 'policy', 'budget', 'price') THEN
        RAISE EXCEPTION 'a bundle manages connectors, tools, agents, versions, principals, groups, the policy, budgets and prices'
            USING ERRCODE = '23514';
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
-- step and, if it revoked an admin grant, leaves at least two admins; a
-- submission is sealed. The journal entry comes last.
CREATE OR REPLACE FUNCTION eacp.change_sets_commit() RETURNS trigger
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
        IF run_stage IS NOT NULL AND EXISTS (
            SELECT 1 FROM eacp.change_set_steps s
             WHERE s.tenant_id = cs.tenant_id AND s.change_set_id = cs.id AND s.stage = run_stage
               AND s.op = 'revoke' AND s.address ~ '^role\.[^.]+\.admin$')
           AND (SELECT count(*) FROM eacp.role_grants g
                  JOIN eacp.principals p ON p.tenant_id = g.tenant_id AND p.id = g.principal_id
                 WHERE g.tenant_id = cs.tenant_id AND g.role = 'admin' AND g.approved_at IS NOT NULL
                   AND g.revoked_at IS NULL AND p.kind = 'human' AND p.disabled_at IS NULL) < 2 THEN
            RAISE EXCEPTION 'change set % would leave fewer than two admins', cs.id
                USING ERRCODE = '23514', HINT = 'admin_floor';
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

-- +goose Down
DROP INDEX eacp.bundle_resources_one_policy;
ALTER TABLE eacp.bundle_resources DROP CONSTRAINT bundle_resources_kind_check;
ALTER TABLE eacp.bundle_resources ADD CONSTRAINT bundle_resources_kind_check
    CHECK (kind IN ('connector', 'tool', 'agent', 'version'));
ALTER TABLE eacp.change_set_refs DROP CONSTRAINT change_set_refs_kind_check;
ALTER TABLE eacp.change_set_refs ADD CONSTRAINT change_set_refs_kind_check CHECK (kind IN ('connector', 'tool',
    'contract', 'agent', 'version', 'allowlist', 'principal', 'group'));
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_op_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_op_check
    CHECK (op IN ('create', 'propose', 'activate', 'transition', 'revoke', 'import'));
ALTER TABLE eacp.change_set_steps DROP CONSTRAINT change_set_steps_address_check;
ALTER TABLE eacp.change_set_steps ADD CONSTRAINT change_set_steps_address_check CHECK (address ~
    '^(connector|tool|contract|agent|version|allowlist)\.[a-z0-9][a-z0-9_-]{0,62}(\.[a-z0-9][a-z0-9_-]{0,62})?$');
```

- [ ] **Step 5: Append the 00020 function bodies to Down**

The Down section must restore `change_set_ref_row`, `bundle_resources_guard` and `change_sets_commit` byte for byte. Run this Python (from the repository root) once:
```python
import re
src = open('migrations/00020_governance_as_code.sql', encoding='utf-8').read()
out = []
for name in ('change_set_ref_row', 'bundle_resources_guard', 'change_sets_commit'):
    m = re.search(r'-- \+goose StatementBegin\n(?:--[^\n]*\n)*CREATE FUNCTION eacp\.' + name + r'\(.*?\n\$\$;\n-- \+goose StatementEnd\n', src, re.S)
    out.append('\n' + m.group(0).replace('CREATE FUNCTION', 'CREATE OR REPLACE FUNCTION', 1))
p = 'migrations/00021_governance_as_code_identity.sql'
with open(p, 'a', encoding='utf-8', newline='') as f:
    f.write(''.join(out))
```

- [ ] **Step 6: Run the tests**

Run: `go test -race -count=1 ./internal/bundle/ ./internal/storage/ ./migrations/`
Expected: PASS, including `TestEveryDownMigrationRestoresThePreviousSchema` and the RLS catalog test (no new table or SECURITY DEFINER function).

- [ ] **Step 7: Commit**

```bash
git add migrations/00021_governance_as_code_identity.sql internal/bundle/schema_phase21_test.go
git commit -m "feat(bundle): migration 00021 - identity, policy, budget and price kinds; admin floor at commit"
```

---

### Task 3: Document sections and their validation

**Files:**
- Create: `internal/bundle/resources.go`
- Modify: `internal/bundle/document.go`
- Test: `internal/bundle/resources_test.go`

**Interfaces:**
- Produces: `Document.{Principals map[string]Principal, Groups map[string]Group, Policy *Policy, Budgets map[string]Budget, Prices map[string]Price}`; types `Principal{Kind, Subject, DisplayName string; Roles []string}`, `Group{DisplayName string; ScheduleWeight int; Members []string}`, `Policy{Content json.RawMessage}`, `Budget{Unit, Parent, Agent string; HardLimit json.Number; SoftLimit *json.Number}`, `Price{Provider, Model, Unit string; InputPerMTok json.Number; CachedPerMTok *json.Number; OutputPerMTok json.Number}`; `const PolicyAddress = "policy.tenant"`; `KindPending = "pending"`, `KindAdminFloor = "admin_floor"`; `amount(json.Number) (string, bool)`, `cmpAmount(a, b string) int`.

- [ ] **Step 1: Write the failing test**

`internal/bundle/resources_test.go`:
```go
package bundle

import (
	"encoding/json"
	"strings"
	"testing"
)

const allowPolicy = `{"format_version":1,"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]}`

const governanceDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "dana@example.com", "display_name": "Dana", "roles": ["auditor"]},
                "ci-bot": {"kind": "service", "display_name": "CI bot", "roles": ["auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana"]}},
 "policy": {"content": ` + allowPolicy + `},
 "budgets": {"root": {"unit": "USD", "hard_limit": 1000},
             "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}},
 "prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD", "input_per_mtok": 2.5, "output_per_mtok": 10}},
 "imports": [{"to": "policy.tenant", "id": "00000000-0000-4000-8000-00000000c001"}]}`

func TestAValidGovernanceDocumentHasNoFindings(t *testing.T) {
	d, err := Decode(json.RawMessage(governanceDoc))
	if err != nil {
		t.Fatal(err)
	}
	if fs := Validate(d, json.RawMessage(governanceDoc)); len(fs) != 0 {
		t.Fatalf("findings = %+v", fs)
	}
}

func TestValidateReportsIdentityPolicyBudgetAndPriceProblems(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, addr string }{
		"service role":  {`"display_name": "CI bot", "roles": ["auditor"]`, `"display_name": "CI bot", "roles": ["admin"]`, "principal.ci-bot"},
		"no subject":    {`"subject": "dana@example.com", `, ``, "principal.dana"},
		"unknown role":  {`"display_name": "Dana", "roles": ["auditor"]`, `"display_name": "Dana", "roles": ["root"]`, "principal.dana"},
		"kind":          {`"kind": "human"`, `"kind": "robot"`, "principal.dana"},
		"weight":        {`"schedule_weight": 2`, `"schedule_weight": 11`, "group.ops"},
		"member twice":  {`"members": ["dana"]`, `"members": ["dana", "dana"]`, "group.ops"},
		"policy":        {`"rules":[{"id":"all","verdict":"allow","reason":"permitted"}]`, `"rules":[]`, PolicyAddress},
		"unit":          {`"unit": "USD", "hard_limit": 1000`, `"unit": "usd", "hard_limit": 1000`, "budget.root"},
		"precision":     {`"hard_limit": 1000`, `"hard_limit": 0.0000001`, "budget.root"},
		"negative":      {`"hard_limit": 200`, `"hard_limit": -1`, "budget.team"},
		"soft zero":     {`"soft_limit": 150`, `"soft_limit": 0`, "budget.team"},
		"cycle":         {`"unit": "USD", "hard_limit": 1000`, `"unit": "USD", "parent": "team", "hard_limit": 1000`, "budget.root"},
		"parent unit":   {`"unit": "USD", "parent": "root"`, `"unit": "EUR", "parent": "root"`, "budget.team"},
		"agent parent":  {`"unit": "USD", "hard_limit": 1000`, `"unit": "USD", "agent": "buyer", "hard_limit": 1000`, "budget.team"},
		"model":         {`"model": "gpt-4.1"`, `"model": ""`, "price.gpt"},
		"price amount":  {`"input_per_mtok": 2.5`, `"input_per_mtok": -2.5`, "price.gpt"},
		"same model":    {`"output_per_mtok": 10}}`, `"output_per_mtok": 10}, "gpt2": {"provider": "openai", "model": "gpt-4.1", "unit": "USD", "input_per_mtok": 1, "output_per_mtok": 1}}`, "price.gpt2"},
		"import a role": {`"to": "policy.tenant"`, `"to": "role.dana.auditor"`, "import.role.dana.auditor"},
	} {
		raw := strings.Replace(governanceDoc, tc.from, tc.to, 1)
		if raw == governanceDoc {
			t.Fatalf("%s: the fixture does not contain %s", name, tc.from)
		}
		d, err := Decode(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := findingKinds(Validate(d, json.RawMessage(raw))); got[tc.addr] != KindInvalid {
			t.Errorf("%s: findings %v, want invalid at %s", name, got, tc.addr)
		}
	}
}

func TestAmountsAreCanonicalNumeric6(t *testing.T) {
	for in, want := range map[string]string{"1000": "1000", "1000.000": "1000", "2.50": "2.5", "0": "0",
		"0.000001": "0.000001", "1e3": "1000"} {
		if got, ok := amount(json.Number(in)); !ok || got != want {
			t.Errorf("amount(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"-1", "0.0000001", "1000000000000000", "", "x"} {
		if _, ok := amount(json.Number(in)); ok {
			t.Errorf("amount(%s) is accepted", in)
		}
	}
	if cmpAmount("2.5", "2.50") != 0 || cmpAmount("10", "9") <= 0 {
		t.Fatal("amounts compare by value")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./internal/bundle/ -run 'GovernanceDocument|IdentityPolicyBudgetAndPrice|CanonicalNumeric6'`
Expected: FAIL to compile: `unknown field "principals"` is a runtime failure, but first `undefined: amount`, `undefined: PolicyAddress`.

- [ ] **Step 3: Write `resources.go`**

`internal/bundle/resources.go`:
```go
package bundle

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"eacp/internal/governance"
)

// Principal is a declared principal. Its name, kind and subject are
// immutable; Roles is the full set of approved roles it should hold.
type Principal struct {
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject,omitempty"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles,omitempty"`
}

// Group is a declared group. ScheduleWeight is set only when the group is
// created; Members is the full set of principals in it.
type Group struct {
	DisplayName    string   `json:"display_name"`
	ScheduleWeight int      `json:"schedule_weight,omitempty"`
	Members        []string `json:"members,omitempty"`
}

// Policy is the tenant's governance policy, a local policy bundle.
type Policy struct {
	Content json.RawMessage `json:"content"`
}

// Budget is a declared budget account. Unit, Parent and Agent are
// immutable; an omitted SoftLimit leaves the current one alone.
type Budget struct {
	Unit      string       `json:"unit"`
	Parent    string       `json:"parent,omitempty"`
	Agent     string       `json:"agent,omitempty"`
	HardLimit json.Number  `json:"hard_limit"`
	SoftLimit *json.Number `json:"soft_limit,omitempty"`
}

// Price is the price that should be in effect for a provider and model. A
// different one is added, effective when it is applied; a bundle never
// removes or backdates a price.
type Price struct {
	Provider      string       `json:"provider"`
	Model         string       `json:"model"`
	Unit          string       `json:"unit"`
	InputPerMTok  json.Number  `json:"input_per_mtok"`
	CachedPerMTok *json.Number `json:"cached_input_per_mtok,omitempty"`
	OutputPerMTok json.Number  `json:"output_per_mtok"`
}

// PolicyAddress is the address of the tenant policy.
const PolicyAddress = "policy.tenant"

// maxPolicy keeps a policy step within a step's 64 KiB payload.
const maxPolicy = 60000

const amountRule = "is a non-negative amount below 10^15 with at most 6 decimals"

var (
	unitRE     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,15}$`)
	providerRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	roleNames  = []string{"admin", "registry_editor", "registry_approver", "operator", "approver", "auditor"}
	million    = big.NewRat(1_000_000, 1)
	amountCap  = new(big.Rat).SetInt64(1_000_000_000_000_000)
)

// amount returns n in the canonical form of a numeric(21,6) column, if the
// column stores it exactly.
func amount(n json.Number) (string, bool) {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 || r.Cmp(amountCap) >= 0 || !new(big.Rat).Mul(r, million).IsInt() {
		return "", false
	}
	s := strings.TrimRight(strings.TrimRight(r.FloatString(6), "0"), ".")
	if s == "" {
		s = "0"
	}
	return s, true
}

// cmpAmount compares two amounts by value.
func cmpAmount(a, b string) int {
	x, okX := new(big.Rat).SetString(a)
	y, okY := new(big.Rat).SetString(b)
	if !okX || !okY {
		return strings.Compare(a, b)
	}
	return x.Cmp(y)
}

// validateGovernance reports the structural problems of the identity,
// policy, budget and price sections as blocking findings.
func validateGovernance(d Document) []Finding {
	var out []Finding
	bad := func(addr, format string, args ...any) {
		out = append(out, Finding{Address: addr, Kind: KindInvalid, Detail: fmt.Sprintf(format, args...)})
	}
	for _, name := range sortedKeys(d.Principals) {
		p, addr := d.Principals[name], "principal."+name
		if !slugRE.MatchString(name) {
			bad(addr, "principal names match %s", slugRE)
		}
		switch p.Kind {
		case "human":
			if s := strings.TrimSpace(p.Subject); len(s) < 3 || len(s) > 320 {
				bad(addr, "a human needs a subject (an IdP subject or email)")
			}
		case "service":
			if p.Subject != "" {
				bad(addr, "a service principal has no subject")
			}
		default:
			bad(addr, "kind is human or service")
		}
		if strings.TrimSpace(p.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		seen := map[string]bool{}
		for _, r := range p.Roles {
			switch {
			case !slices.Contains(roleNames, r):
				bad(addr, "unknown role %q", r)
			case p.Kind == "service" && r != "auditor":
				bad(addr, "a service principal may hold only auditor (ADR-003 §1)")
			case seen[r]:
				bad(addr, "role %q is listed twice", r)
			}
			seen[r] = true
		}
	}
	for _, name := range sortedKeys(d.Groups) {
		g, addr := d.Groups[name], "group."+name
		if !slugRE.MatchString(name) {
			bad(addr, "group names match %s", slugRE)
		}
		if strings.TrimSpace(g.DisplayName) == "" {
			bad(addr, "display_name is required")
		}
		if g.ScheduleWeight != 0 && (g.ScheduleWeight < 1 || g.ScheduleWeight > 10) {
			bad(addr, "schedule_weight is 1 to 10")
		}
		seen := map[string]bool{}
		for _, m := range g.Members {
			switch {
			case !slugRE.MatchString(m):
				bad(addr, "member %q is not a principal name", m)
			case seen[m]:
				bad(addr, "%s is listed twice", m)
			}
			seen[m] = true
		}
	}
	if d.Policy != nil {
		if len(d.Policy.Content) > maxPolicy {
			bad(PolicyAddress, "the policy is over %d bytes", maxPolicy)
		} else if err := governance.ValidatePolicy(d.Policy.Content); err != nil {
			bad(PolicyAddress, "%v", err)
		}
	}
	for _, name := range sortedKeys(d.Budgets) {
		b, addr := d.Budgets[name], "budget."+name
		if !toolNameRE.MatchString(name) {
			bad(addr, "budget names match %s", toolNameRE)
		}
		if !unitRE.MatchString(b.Unit) {
			bad(addr, "unit matches %s", unitRE)
		}
		if _, ok := amount(b.HardLimit); !ok {
			bad(addr, "hard_limit %s", amountRule)
		}
		if b.SoftLimit != nil {
			if s, ok := amount(*b.SoftLimit); !ok || s == "0" {
				bad(addr, "soft_limit is a positive amount below 10^15 with at most 6 decimals")
			}
		}
		if b.Agent != "" && !slugRE.MatchString(b.Agent) {
			bad(addr, "agent %q is not an agent name", b.Agent)
		}
		if par, ok := d.Budgets[b.Parent]; b.Parent != "" && ok {
			if par.Unit != b.Unit {
				bad(addr, "parent %s is in %s, not %s", b.Parent, par.Unit, b.Unit)
			}
			if par.Agent != "" {
				bad(addr, "parent %s belongs to agent %s; an agent's account has no children", b.Parent, par.Agent)
			}
		}
		for cur, n := b.Parent, 0; cur != "" && n <= len(d.Budgets); cur, n = d.Budgets[cur].Parent, n+1 {
			if cur == name {
				bad(addr, "budget parents form a cycle")
				break
			}
		}
	}
	keys := map[string]string{}
	for _, name := range sortedKeys(d.Prices) {
		p, addr := d.Prices[name], "price."+name
		if !toolNameRE.MatchString(name) {
			bad(addr, "price names match %s", toolNameRE)
		}
		if !providerRE.MatchString(p.Provider) {
			bad(addr, "provider matches %s", providerRE)
		}
		if len(p.Model) == 0 || len(p.Model) > 256 || strings.ContainsFunc(p.Model, unicode.IsControl) {
			bad(addr, "model is 1 to 256 characters without control characters")
		}
		if !unitRE.MatchString(p.Unit) {
			bad(addr, "unit matches %s", unitRE)
		}
		for _, f := range []struct {
			field string
			n     *json.Number
		}{{"input_per_mtok", &p.InputPerMTok}, {"cached_input_per_mtok", p.CachedPerMTok},
			{"output_per_mtok", &p.OutputPerMTok}} {
			if f.n != nil {
				if _, ok := amount(*f.n); !ok {
					bad(addr, "%s %s", f.field, amountRule)
				}
			}
		}
		key := p.Provider + " " + p.Model
		if other, dup := keys[key]; dup {
			bad(addr, "price.%s already prices %s", other, key)
		}
		keys[key] = name
	}
	return out
}

// declaredGovernance reports whether d declares an identity, policy, budget
// or price address.
func declaredGovernance(d Document, kind, name string) bool {
	switch kind {
	case "principal":
		_, ok := d.Principals[name]
		return ok
	case "role":
		p, role, _ := strings.Cut(name, ".")
		return slices.Contains(d.Principals[p].Roles, role)
	case "group":
		_, ok := d.Groups[name]
		return ok
	case "member":
		g, p, _ := strings.Cut(name, ".")
		return slices.Contains(d.Groups[g].Members, p)
	case "policy":
		return name == "tenant" && d.Policy != nil
	case "budget":
		_, ok := d.Budgets[name]
		return ok
	case "price":
		_, ok := d.Prices[name]
		return ok
	}
	return false
}
```

- [ ] **Step 4: Wire it into `document.go`**

Replace the `Document` struct:
```go
type Document struct {
	Principals map[string]Principal `json:"principals,omitempty"`
	Groups     map[string]Group     `json:"groups,omitempty"`
	Connectors map[string]Connector `json:"connectors,omitempty"`
	Agents     map[string]Agent     `json:"agents,omitempty"`
	Policy     *Policy              `json:"policy,omitempty"`
	Budgets    map[string]Budget    `json:"budgets,omitempty"`
	Prices     map[string]Price     `json:"prices,omitempty"`
	Imports    []Import             `json:"imports,omitempty"`
}
```
Add to the finding kinds (`pending` and `admin_floor` block):
```go
	KindPending             = "pending"
	KindAdminFloor          = "admin_floor"
```
Replace `importRE`:
```go
	importRE   = regexp.MustCompile(`^(connector|tool|agent|version|principal|group|budget|price|policy)\.`)
```
and its message in `Validate`:
```go
			bad(addr, "imports adopt a connector, tool, agent, version, principal, group, budget, price or the policy")
```
In `Validate`, before the `seen := map[string]bool{}` of the imports loop, add:
```go
	out = append(out, validateGovernance(d)...)
```
In `declared`, replace the final `return false` with:
```go
	return declaredGovernance(d, kind, name)
```

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/bundle/ -run 'Document|Decode|Validate|Governance|Amounts|Orphans|Diff'`
Expected: PASS (the new tests and every Phase 20 document and diff test).

- [ ] **Step 6: Commit**

```bash
git add internal/bundle/resources.go internal/bundle/resources_test.go internal/bundle/document.go
git commit -m "feat(bundle): principals, groups, policy, budgets and prices in the document"
```

---

### Task 4: State and the identity planner

**Files:**
- Create: `internal/bundle/state_governance.go`, `internal/bundle/plan_identity.go`
- Modify: `internal/bundle/state.go`, `internal/bundle/plan.go`, `internal/bundle/plan_test.go` (`empty()`)
- Test: `internal/bundle/plan_identity_test.go`, `internal/bundle/state_test.go`

**Interfaces:**
- Consumes: Task 3 document types.
- Produces: `State.{AddressElsewhere map[string]string, TenantID uuid.UUID, People map[string]PrincipalState, GroupRows map[string]GroupState, Policy PolicyState, Budgets map[string]BudgetState, Prices map[string]PriceState}`; `PrincipalState{ID; Kind, Subject, DisplayName string; Disabled bool; Grants map[string]GrantState}`, `GrantState{ID; Approved bool}`, `GroupState{ID; DisplayName string; Weight int; Members map[string]uuid.UUID}`, `PolicyState{ID *uuid.UUID; Version int; Content json.RawMessage}`, `BudgetState{ID; Unit string; ParentID, AgentID *uuid.UUID; HardLimit string; SoftLimit *string; OpenProposal *uuid.UUID}`, `PriceState{ID; Unit, Input, Output string; Cached *string}`; `OpSet = "set"`; `Payload` fields `Principal *registry.NewPrincipal`, `Group *GroupSpec`, `Policy json.RawMessage`, `Budget *budget.NewAccount`, `Limit string`, `SoftLimit *string`, `Price *finops.NewPrice`, `Other string`, `OtherID *uuid.UUID`; `GroupSpec{Name, DisplayName string; Weight int}`; planner fields `late []Step`, `admins int`, `revokedAdmins []string`.

- [ ] **Step 1: Write the failing tests**

`internal/bundle/plan_identity_test.go`:
```go
package bundle

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	alice      = uuid.MustParse("00000000-0000-4000-8000-00000000a001")
	bob        = uuid.MustParse("00000000-0000-4000-8000-00000000a002")
	dana       = uuid.MustParse("00000000-0000-4000-8000-00000000a003")
	aliceAdmin = uuid.MustParse("00000000-0000-4000-8000-00000000a0a1")
	bobAdmin   = uuid.MustParse("00000000-0000-4000-8000-00000000a0a2")
	danaAudit  = uuid.MustParse("00000000-0000-4000-8000-00000000a0a3")
	danaOps    = uuid.MustParse("00000000-0000-4000-8000-00000000a0a4")
	opsID      = uuid.MustParse("00000000-0000-4000-8000-00000000b0b1")
	opsAlice   = uuid.MustParse("00000000-0000-4000-8000-00000000b0b2")
	opsDana    = uuid.MustParse("00000000-0000-4000-8000-00000000b0b3")
)

// admins is empty() with alice and bob as the tenant's two admins.
func admins() State {
	st := empty()
	st.People["alice"] = PrincipalState{ID: alice, Kind: "human", Subject: "alice@example.com", DisplayName: "Alice",
		Grants: map[string]GrantState{"admin": {ID: aliceAdmin, Approved: true}}}
	st.People["bob"] = PrincipalState{ID: bob, Kind: "human", Subject: "bob@example.com", DisplayName: "Bob",
		Grants: map[string]GrantState{"admin": {ID: bobAdmin, Approved: true}}}
	st.Principals["alice"], st.Principals["bob"] = alice, bob
	return st
}

const identityDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "Dana@Example.com", "display_name": "Dana",
                         "roles": ["operator", "auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana", "alice"]}}}`

// identityApplied is admins() after identityDoc was applied.
func identityApplied() State {
	st := admins()
	st.People["dana"] = PrincipalState{ID: dana, Kind: "human", Subject: "dana@example.com", DisplayName: "Dana",
		Grants: map[string]GrantState{"auditor": {ID: danaAudit, Approved: true}, "operator": {ID: danaOps, Approved: true}}}
	st.Principals["dana"] = dana
	st.GroupRows["ops"] = GroupState{ID: opsID, DisplayName: "Ops", Weight: 2,
		Members: map[string]uuid.UUID{"alice": opsAlice, "dana": opsDana}}
	st.Groups["ops"] = opsID
	st.Managed["principal.dana"], st.Managed["group.ops"] = dana, opsID
	return st
}

func TestIdentityIsCreatedBeforeItIsGrantedOrJoined(t *testing.T) {
	d := diff("people", cs, mustDoc(t, identityDoc), admins(), false)
	wantOps(t, d,
		"1 submit create principal.dana",
		"2 submit propose role.dana.auditor",
		"3 submit propose role.dana.operator",
		"4 submit create group.ops",
		"5 submit create member.ops.alice",
		"6 submit create member.ops.dana",
		"7 approve activate role.dana.auditor",
		"8 approve activate role.dana.operator")
	if p := d.Steps[0].Payload.Principal; p == nil || p.Subject != "dana@example.com" || p.Name != "dana" {
		t.Fatalf("principal payload = %+v", p)
	}
	if p := d.Steps[1].Payload; p.Name != "auditor" || p.Parent != "principal.dana" {
		t.Fatalf("role payload = %+v", p)
	}
	if p := d.Steps[4].Payload; p.OtherID == nil || *p.OtherID != alice || p.Parent != "group.ops" {
		t.Fatalf("member payload = %+v", p)
	}
	if p := d.Steps[5].Payload; p.Other != "principal.dana" {
		t.Fatalf("member payload = %+v", p)
	}
	if p := d.Steps[6].Payload; p.ProposalStep != 2 {
		t.Fatalf("activation payload = %+v", p)
	}
	wantFinding(t, d, "principal.alice", KindUnmanagedReference)
}

func TestTheAppliedIdentityPlansNothing(t *testing.T) {
	d := diff("people", cs, mustDoc(t, identityDoc), identityApplied(), false)
	wantOps(t, d)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
}

func TestPruneRevokesUndeclaredRolesAndMembersLast(t *testing.T) {
	raw := strings.Replace(identityDoc, `"roles": ["operator", "auditor"]`, `"roles": ["auditor"]`, 1)
	raw = strings.Replace(raw, `"members": ["dana", "alice"]`, `"members": ["dana"]`, 1)
	d := diff("people", cs, mustDoc(t, raw), identityApplied(), false)
	wantOps(t, d)
	wantFinding(t, d, "role.dana.operator", KindOrphan)
	wantFinding(t, d, "member.ops.alice", KindOrphan)
	d = diff("people", cs, mustDoc(t, raw), identityApplied(), true)
	wantOps(t, d, "1 approve revoke role.dana.operator", "2 approve revoke member.ops.alice")
	if p := d.Steps[0].Payload; p.ObjectID == nil || *p.ObjectID != danaOps {
		t.Fatalf("revoke payload = %+v", p)
	}
}

func TestAPendingGrantBlocks(t *testing.T) {
	st := identityApplied()
	st.People["dana"].Grants["operator"] = GrantState{ID: danaOps, Approved: false}
	d := diff("people", cs, mustDoc(t, identityDoc), st, false)
	wantFinding(t, d, "role.dana.operator", KindPending)
	for _, f := range d.Findings {
		if f.Kind == KindPending && !strings.Contains(f.Detail, danaOps.String()) {
			t.Fatalf("a pending finding names the grant to decide: %+v", f)
		}
	}
}

const adminsDoc = `{"principals": {
 "alice": {"kind": "human", "subject": "alice@example.com", "display_name": "Alice", "roles": ALICE},
 "bob": {"kind": "human", "subject": "bob@example.com", "display_name": "Bob", "roles": ["admin"]}DAVE}}`

func TestTheAdminFloorBlocksAPlanThatLeavesOneAdmin(t *testing.T) {
	st := admins()
	st.Managed["principal.alice"], st.Managed["principal.bob"] = alice, bob
	lone := strings.NewReplacer("ALICE", "[]", "DAVE", "").Replace(adminsDoc)
	d := diff("admins", cs, mustDoc(t, lone), st, true)
	wantFinding(t, d, "role.alice.admin", KindAdminFloor)
	// Without prune nothing is revoked and there is no floor to keep.
	d = diff("admins", cs, mustDoc(t, lone), st, false)
	wantFinding(t, d, "role.alice.admin", KindOrphan)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
	// A new admin in the same change set keeps two.
	withDave := strings.NewReplacer("ALICE", "[]", "DAVE", `,
 "dave": {"kind": "human", "subject": "dave@example.com", "display_name": "Dave", "roles": ["admin"]}`).Replace(adminsDoc)
	d = diff("admins", cs, mustDoc(t, withDave), st, true)
	if blocked(d.Findings) {
		t.Fatalf("findings = %+v", d.Findings)
	}
	wantOps(t, d,
		"1 submit create principal.dave",
		"2 submit propose role.dave.admin",
		"3 approve activate role.dave.admin",
		"4 approve revoke role.alice.admin")
}

func TestIdentityFieldsAreImmutableAndDisabledPrincipalsAreLeftAlone(t *testing.T) {
	raw := strings.Replace(identityDoc, `"display_name": "Dana"`, `"display_name": "Dana B"`, 1)
	wantFinding(t, diff("people", cs, mustDoc(t, raw), identityApplied(), false), "principal.dana", KindUnsupported)
	raw = strings.Replace(identityDoc, `"schedule_weight": 2`, `"schedule_weight": 3`, 1)
	wantFinding(t, diff("people", cs, mustDoc(t, raw), identityApplied(), false), "group.ops", KindUnsupported)
	st := identityApplied()
	x := st.People["dana"]
	x.Disabled = true
	st.People["dana"] = x
	wantFinding(t, diff("people", cs, mustDoc(t, identityDoc), st, false), "principal.dana", KindUnsupported)
}

func TestAnAgentMayBeOwnedByAGroupCreatedInTheSameBundle(t *testing.T) {
	raw := `{"groups": {"ops": {"display_name": "Ops"}},
	 "agents": {"bot": {"display_name": "Bot", "environment": "production", "risk_class": "low",
	   "owner": {"group": "ops"}, "version": {"runtime": "python", "code_ref": "git:1"}}}}`
	d := diff("ops", cs, mustDoc(t, raw), empty(), false)
	wantOps(t, d, "1 submit create group.ops", "2 submit create agent.bot", "3 submit create version.bot")
	if p := d.Steps[0].Payload.Group; p == nil || p.Weight != 1 {
		t.Fatalf("group payload = %+v", p)
	}
	if p := d.Steps[1].Payload; p.Other != "group.ops" {
		t.Fatalf("agent payload = %+v", p)
	}
}

func TestExistingPrincipalsAreImportedByID(t *testing.T) {
	raw := strings.NewReplacer("ALICE", `["admin"]`, "DAVE", "").Replace(adminsDoc)
	d := diff("admins", cs, mustDoc(t, raw), admins(), false)
	wantFinding(t, d, "principal.alice", KindUnmanaged)
	raw = strings.TrimSuffix(raw, "}") + `, "imports": [{"to": "principal.alice", "id": "` + alice.String() +
		`"}, {"to": "principal.bob", "id": "` + bob.String() + `"}]}`
	wantOps(t, diff("admins", cs, mustDoc(t, raw), admins(), false),
		"1 submit import principal.alice", "2 submit import principal.bob")
}
```

`internal/bundle/state_test.go`:
```go
package bundle

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

func TestLoadStateReadsPeopleGroupsPolicyBudgetsAndPrices(t *testing.T) {
	f := registrytest.New(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	g := f.ID(t, "alice", `INSERT INTO eacp.groups (tenant_id, name, display_name, schedule_weight)
		VALUES (eacp.current_tenant_id(), 'ops', 'Ops', 3) RETURNING id`)
	f.ID(t, "alice", `INSERT INTO eacp.group_memberships (tenant_id, group_id, principal_id)
		VALUES (eacp.current_tenant_id(), $1, $2) RETURNING id`, g, f.P["carol"])
	pol := f.ID(t, "alice", `INSERT INTO eacp.policy_bundles (tenant_id, content)
		VALUES (eacp.current_tenant_id(), $1::jsonb) RETURNING id`, allowPolicy)
	must(f.Exec("bob", `UPDATE eacp.tenant_policy_pointer SET current_bundle_id = $1, activation_reason = 'go'
		WHERE tenant_id = eacp.current_tenant_id()`, pol))
	acct := f.ID(t, "alice", `INSERT INTO eacp.budget_accounts (tenant_id, name, unit)
		VALUES (eacp.current_tenant_id(), 'ops', 'USD') RETURNING id`)
	must(f.Exec("alice", `INSERT INTO eacp.budget_soft_limits (tenant_id, account_id, monthly_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 150, 'watch')`, acct))
	open := f.ID(t, "alice", `INSERT INTO eacp.budget_limit_changes (tenant_id, account_id, new_limit, reason)
		VALUES (eacp.current_tenant_id(), $1, 100, 'fund') RETURNING id`, acct)
	f.ID(t, "alice", `INSERT INTO eacp.model_prices (tenant_id, provider, model, unit, input_per_mtok, output_per_mtok, reason)
		VALUES (eacp.current_tenant_id(), 'openai', 'gpt-4.1', 'USD', 2.5, 10, 'card') RETURNING id`)

	var st State
	must(storage.InTenantSnapshotTx(ctx, f.App, f.Tenant.String(), func(tx pgx.Tx) error {
		var err error
		st, err = loadState(ctx, tx, "people")
		return err
	}))
	if st.TenantID != f.Tenant {
		t.Fatalf("tenant = %s", st.TenantID)
	}
	if g := st.People["alice"].Grants["admin"]; !g.Approved || st.People["ci"].Kind != "service" {
		t.Fatalf("people = %+v", st.People)
	}
	if ops := st.GroupRows["ops"]; ops.Weight != 3 || ops.Members["carol"] == [16]byte{} {
		t.Fatalf("groups = %+v", st.GroupRows)
	}
	if st.Policy.ID == nil || *st.Policy.ID != pol || !strings.Contains(string(st.Policy.Content), "permitted") {
		t.Fatalf("policy = %+v", st.Policy)
	}
	b := st.Budgets["ops"]
	if b.HardLimit != "0" || b.SoftLimit == nil || *b.SoftLimit != "150" || b.OpenProposal == nil || *b.OpenProposal != open {
		t.Fatalf("budget = %+v", b)
	}
	if p := st.Prices["openai gpt-4.1"]; p.Input != "2.5" || p.Output != "10" || p.Cached != nil || p.Unit != "USD" {
		t.Fatalf("prices = %+v", st.Prices)
	}
}
```

In `internal/bundle/plan_test.go`, replace `empty()` so every state map exists and carol is a person:
```go
var tenantID = uuid.MustParse("00000000-0000-4000-8000-00000000e001")

func empty() State {
	return State{Managed: map[string]uuid.UUID{}, ManagedElsewhere: map[uuid.UUID]string{},
		AddressElsewhere: map[string]string{}, TenantID: tenantID,
		Connectors: map[string]ConnectorState{}, Agents: map[string]AgentState{},
		Principals: map[string]uuid.UUID{"carol": carol}, Groups: map[string]uuid.UUID{},
		People: map[string]PrincipalState{"carol": {ID: carol, Kind: "human", Subject: "carol@example.com",
			DisplayName: "Carol", Grants: map[string]GrantState{}}},
		GroupRows: map[string]GroupState{}, Budgets: map[string]BudgetState{}, Prices: map[string]PriceState{}}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/bundle/ -run 'Identity|AppliedIdentity|PendingGrant|AdminFloor|OwnedByAGroup|ImportedByID|LoadState'`
Expected: FAIL to compile: `unknown field AddressElsewhere in struct literal`, `undefined: PrincipalState`.

- [ ] **Step 3: Write `state_governance.go`**

```go
package bundle

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PrincipalState is a principal and its live (unrevoked) grants by role.
type PrincipalState struct {
	ID                         uuid.UUID
	Kind, Subject, DisplayName string
	Disabled                   bool
	Grants                     map[string]GrantState
}

// GrantState is a live grant; it is a proposal until it is approved.
type GrantState struct {
	ID       uuid.UUID
	Approved bool
}

// GroupState is a group and its live memberships by principal name.
type GroupState struct {
	ID          uuid.UUID
	DisplayName string
	Weight      int
	Members     map[string]uuid.UUID
}

// PolicyState is the tenant's active policy version, if it has one.
type PolicyState struct {
	ID      *uuid.UUID
	Version int
	Content json.RawMessage
}

// BudgetState is an account, its limits and its open increase. It holds no
// counters: they move with every action and never shape a plan.
type BudgetState struct {
	ID                uuid.UUID
	Unit              string
	ParentID, AgentID *uuid.UUID
	HardLimit         string
	SoftLimit         *string
	OpenProposal      *uuid.UUID
}

// PriceState is the price in effect for a provider and model.
type PriceState struct {
	ID                  uuid.UUID
	Unit, Input, Output string
	Cached              *string
}

// loadGovernance reads the tenant's people, groups, policy, budgets and
// prices into st, in the plan's snapshot.
func loadGovernance(ctx context.Context, tx pgx.Tx, st *State) error {
	if err := tx.QueryRow(ctx, `SELECT eacp.current_tenant_id()`).Scan(&st.TenantID); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, name, kind, COALESCE(subject, ''), display_name, disabled_at IS NOT NULL
		FROM eacp.principals`)
	if err != nil {
		return err
	}
	var ps PrincipalState
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&ps.ID, &name, &ps.Kind, &ps.Subject, &ps.DisplayName, &ps.Disabled},
		func() error {
			x := ps
			x.Grants = map[string]GrantState{}
			st.People[name] = x
			return nil
		}); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT p.name, g.role, g.id, g.approved_at IS NOT NULL FROM eacp.role_grants g
		JOIN eacp.principals p ON p.tenant_id = g.tenant_id AND p.id = g.principal_id WHERE g.revoked_at IS NULL`)
	if err != nil {
		return err
	}
	var role string
	var g GrantState
	if _, err := pgx.ForEachRow(rows, []any{&name, &role, &g.ID, &g.Approved}, func() error {
		st.People[name].Grants[role] = g
		return nil
	}); err != nil {
		return err
	}

	rows, err = tx.Query(ctx, `SELECT id, name, display_name, schedule_weight FROM eacp.groups`)
	if err != nil {
		return err
	}
	var gs GroupState
	if _, err := pgx.ForEachRow(rows, []any{&gs.ID, &name, &gs.DisplayName, &gs.Weight}, func() error {
		x := gs
		x.Members = map[string]uuid.UUID{}
		st.GroupRows[name] = x
		return nil
	}); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT g.name, p.name, m.id FROM eacp.group_memberships m
		JOIN eacp.groups g ON g.tenant_id = m.tenant_id AND g.id = m.group_id
		JOIN eacp.principals p ON p.tenant_id = m.tenant_id AND p.id = m.principal_id
		WHERE m.removed_at IS NULL`)
	if err != nil {
		return err
	}
	var member string
	var mid uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&name, &member, &mid}, func() error {
		st.GroupRows[name].Members[member] = mid
		return nil
	}); err != nil {
		return err
	}

	var content string
	err = tx.QueryRow(ctx, `SELECT ptr.current_bundle_id, COALESCE(pb.version, 0), COALESCE(pb.content::text, '')
		FROM eacp.tenant_policy_pointer ptr
		LEFT JOIN eacp.policy_bundles pb ON pb.tenant_id = ptr.tenant_id AND pb.id = ptr.current_bundle_id`).
		Scan(&st.Policy.ID, &st.Policy.Version, &content)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	case st.Policy.ID != nil:
		st.Policy.Content = json.RawMessage(content)
	}

	rows, err = tx.Query(ctx, `SELECT b.id, b.name, b.unit, b.parent_id, b.agent_id, trim_scale(b.hard_limit)::text,
		trim_scale(l.monthly_limit)::text,
		(SELECT c.id FROM eacp.budget_limit_changes c
		  WHERE c.tenant_id = b.tenant_id AND c.account_id = b.id AND c.state = 'PROPOSED'
		  ORDER BY c.proposed_at DESC LIMIT 1)
		FROM eacp.budget_accounts b
		LEFT JOIN eacp.budget_soft_limits l ON l.tenant_id = b.tenant_id AND l.account_id = b.id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var b BudgetState
		if err := rows.Scan(&b.ID, &name, &b.Unit, &b.ParentID, &b.AgentID, &b.HardLimit, &b.SoftLimit,
			&b.OpenProposal); err != nil {
			rows.Close()
			return err
		}
		st.Budgets[name] = b
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query(ctx, `SELECT DISTINCT ON (provider, model) id, provider, model, unit,
		trim_scale(input_per_mtok)::text, trim_scale(cached_input_per_mtok)::text, trim_scale(output_per_mtok)::text
		FROM eacp.model_prices WHERE effective_from <= now()
		ORDER BY provider, model, effective_from DESC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p PriceState
		var provider, model string
		if err := rows.Scan(&p.ID, &provider, &model, &p.Unit, &p.Input, &p.Cached, &p.Output); err != nil {
			rows.Close()
			return err
		}
		st.Prices[provider+" "+model] = p
	}
	return rows.Err()
}
```

- [ ] **Step 4: Extend `state.go`**

Add to `State`:
```go
	AddressElsewhere map[string]string        // address -> the other bundle managing it
	TenantID         uuid.UUID                 // the tenant planned
	People           map[string]PrincipalState // every principal by name, disabled ones too
	GroupRows        map[string]GroupState     // by name
	Policy           PolicyState               // the active policy version, if any
	Budgets          map[string]BudgetState    // by name
	Prices           map[string]PriceState     // the price in effect, by provider + " " + model
```
In `loadState`, initialise the new maps in the `st := State{...}` literal:
```go
		AddressElsewhere: map[string]string{}, People: map[string]PrincipalState{}, GroupRows: map[string]GroupState{},
		Budgets: map[string]BudgetState{}, Prices: map[string]PriceState{},
```
record other bundles' addresses in the `bundle_resources` loop:
```go
		} else {
			st.ManagedElsewhere[id] = owner
			st.AddressElsewhere[addr] = owner
		}
```
and end `loadState` with the governance state:
```go
	if _, err := pgx.ForEachRow(rows, []any{&name, &id}, func() error { st.Groups[name] = id; return nil }); err != nil {
		return st, err
	}
	return st, loadGovernance(ctx, tx, &st)
}
```

- [ ] **Step 5: Extend `plan.go`**

Imports gain `"eacp/internal/budget"` and `"eacp/internal/finops"`. Add the op:
```go
	OpSet        = "set"
```
Add to `Payload` (after `ObjectID`):
```go
	Principal *registry.NewPrincipal `json:"principal,omitempty"`
	Group     *GroupSpec             `json:"group,omitempty"`
	Policy    json.RawMessage        `json:"policy,omitempty"`
	Budget    *budget.NewAccount     `json:"budget,omitempty"`
	Limit     string                 `json:"limit,omitempty"`
	SoftLimit *string                `json:"soft_limit,omitempty"`
	Price     *finops.NewPrice       `json:"price,omitempty"`
	Other     string                 `json:"other,omitempty"`
	OtherID   *uuid.UUID             `json:"other_id,omitempty"`
```
and update the `Payload` doc comment: `Other`/`OtherID` name a second object the same way (a membership's principal, an agent's owner, an account's agent).

Add after `Payload`:
```go
// GroupSpec is a group to create.
type GroupSpec struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Weight      int    `json:"schedule_weight"`
}
```
Add to `planner`:
```go
	late          []Step   // approve-stage prunes of grants and memberships, after every other step
	admins        int      // admin grants this plan proposes for human principals
	revokedAdmins []string // the admin grants this plan revokes, by address
```
In `diff`, plan identity first and check the floor last:
```go
	p.imports()
	for _, name := range sortedKeys(doc.Principals) {
		p.principal(name)
	}
	for _, name := range sortedKeys(doc.Groups) {
		p.group(name)
	}
	for _, name := range sortedKeys(doc.Connectors) {
		p.connector(name)
	}
	for _, name := range sortedKeys(doc.Agents) {
		p.agent(name)
	}
	p.orphans()
	p.adminFloor()

	steps := append(append(p.submit, p.approve...), p.late...)
```
Make `names` a planner method (the call in `imports` becomes `p.names(kind, name, im.ID)`) with the new kinds:
```go
// names reports whether id is the object that kind and name denote.
func (p *planner) names(kind, name string, id uuid.UUID) bool {
	st := p.st
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
	case "principal":
		return st.People[name].ID == id && id != uuid.Nil
	case "group":
		return st.GroupRows[name].ID == id && id != uuid.Nil
	case "budget":
		return st.Budgets[name].ID == id && id != uuid.Nil
	case "policy":
		return name == "tenant" && st.Policy.ID != nil && *st.Policy.ID == id
	case "price":
		w := p.doc.Prices[name]
		cur, ok := st.Prices[w.Provider+" "+w.Model]
		return ok && cur.ID == id
	}
	return false
}
```
Replace `owner` so an owner created by this bundle is named by address, and pass it into the agent step:
```go
// owner resolves the agent's owner: an existing principal or group, or one
// this bundle creates, named by its address until the step runs.
func (p *planner) owner(addr string, o Owner, na *registry.NewAgent) (string, bool) {
	if o.Principal != "" {
		if id, ok := p.st.Principals[o.Principal]; ok {
			na.OwnerPrincipalID = id
			p.ref("principal", id)
			return "", true
		}
		if _, declared := p.doc.Principals[o.Principal]; declared {
			if _, exists := p.st.People[o.Principal]; !exists {
				return "principal." + o.Principal, true
			}
		}
		p.find(addr, KindUnresolvedReference, "no enabled principal %q", o.Principal)
		return "", false
	}
	if id, ok := p.st.Groups[o.Group]; ok {
		na.OwnerGroupID = id
		p.ref("group", id)
		return "", true
	}
	if _, declared := p.doc.Groups[o.Group]; declared {
		return "group." + o.Group, true
	}
	p.find(addr, KindUnresolvedReference, "no group %q", o.Group)
	return "", false
}
```
In `agent`:
```go
	other, ok := p.owner(addr, want.Owner, &na)
	if !ok {
		return
	}
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Agent: &na, Other: other})
	} else {
```

- [ ] **Step 6: Write `plan_identity.go`**

```go
package bundle

import (
	"strings"

	"github.com/google/uuid"

	"eacp/internal/registry"
)

// principal plans a declared principal and its roles. Every live grant of a
// principal this bundle manages is the bundle's: prune revokes the ones it no
// longer declares, after every other step of the approval.
func (p *planner) principal(name string) {
	want, addr := p.doc.Principals[name], "principal."+name
	cur, exists := p.st.People[name]
	var id uuid.UUID
	if !exists {
		p.add(StageSubmit, addr, OpCreate, Payload{Principal: &registry.NewPrincipal{Kind: want.Kind, Name: name,
			Subject: strings.ToLower(strings.TrimSpace(want.Subject)), DisplayName: want.DisplayName}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("principal", id)
		if cur.Disabled {
			p.find(addr, KindUnsupported, "the principal is disabled; a bundle never enables or disables anyone")
			return
		}
		for _, f := range []struct{ field, have, want string }{{"kind", cur.Kind, want.Kind},
			{"subject", cur.Subject, strings.ToLower(strings.TrimSpace(want.Subject))},
			{"display_name", cur.DisplayName, want.DisplayName}} {
			if f.have != f.want {
				p.find(addr, KindUnsupported, "%s is immutable: %q in the registry, %q in the bundle", f.field,
					f.have, f.want)
			}
		}
	}
	wanted := map[string]bool{}
	for _, r := range want.Roles {
		wanted[r] = true
	}
	for _, role := range sortedKeys(wanted) {
		raddr := "role." + name + "." + role
		if g, ok := cur.Grants[role]; ok {
			p.ref("role", g.ID)
			if !g.Approved {
				p.find(raddr, KindPending, "grant %s was proposed outside this bundle: approve or revoke it through the API first", g.ID)
			}
			continue
		}
		par, parID := parent(addr, id)
		n := p.add(StageSubmit, raddr, OpPropose, Payload{Name: role, Parent: par, ParentID: parID})
		p.add(StageApprove, raddr, OpActivate, Payload{ProposalStep: n})
		if role == "admin" && want.Kind == "human" {
			p.admins++
		}
	}
	for _, role := range sortedKeys(cur.Grants) {
		if wanted[role] {
			continue
		}
		g, raddr := cur.Grants[role], "role."+name+"."+role
		p.ref("role", g.ID)
		switch {
		case !p.prune:
			p.find(raddr, KindOrphan, "role %s is no longer declared; prune revokes it", role)
		case !g.Approved:
			p.find(raddr, KindPending, "grant %s was proposed outside this bundle: approve or revoke it through the API first", g.ID)
		default:
			gid := g.ID
			p.late = append(p.late, Step{Address: raddr, Op: OpRevoke, Stage: StageApprove,
				Payload: Payload{ObjectID: &gid, Reason: p.reason + " prune"}})
			if role == "admin" && cur.Kind == "human" {
				p.revokedAdmins = append(p.revokedAdmins, raddr)
			}
		}
	}
}

// group plans a declared group and its members. Every live membership of a
// group this bundle manages is the bundle's.
func (p *planner) group(name string) {
	want, addr := p.doc.Groups[name], "group."+name
	cur, exists := p.st.GroupRows[name]
	var id uuid.UUID
	if !exists {
		weight := want.ScheduleWeight
		if weight == 0 {
			weight = 1
		}
		p.add(StageSubmit, addr, OpCreate, Payload{Group: &GroupSpec{Name: name, DisplayName: want.DisplayName,
			Weight: weight}})
	} else {
		if !p.owned(addr, cur.ID) {
			return
		}
		id = cur.ID
		p.ref("group", id)
		if cur.DisplayName != want.DisplayName {
			p.find(addr, KindUnsupported, "display_name is immutable: %q in the registry, %q in the bundle",
				cur.DisplayName, want.DisplayName)
		}
		if want.ScheduleWeight != 0 && want.ScheduleWeight != cur.Weight {
			p.find(addr, KindUnsupported, "schedule_weight is set only when the group is created (it is %d)", cur.Weight)
		}
	}
	wanted := map[string]bool{}
	for _, m := range want.Members {
		wanted[m] = true
	}
	for _, m := range sortedKeys(wanted) {
		maddr := "member." + name + "." + m
		if mid, ok := cur.Members[m]; ok {
			p.ref("member", mid)
			continue
		}
		other, otherID, ok := p.principalRef(maddr, m)
		if !ok {
			continue
		}
		par, parID := parent(addr, id)
		p.add(StageSubmit, maddr, OpCreate, Payload{Parent: par, ParentID: parID, Other: other, OtherID: otherID})
	}
	for _, m := range sortedKeys(cur.Members) {
		if wanted[m] {
			continue
		}
		mid, maddr := cur.Members[m], "member."+name+"."+m
		p.ref("member", mid)
		if !p.prune {
			p.find(maddr, KindOrphan, "%s is no longer a declared member; prune removes it", m)
			continue
		}
		p.late = append(p.late, Step{Address: maddr, Op: OpRevoke, Stage: StageApprove,
			Payload: Payload{ObjectID: &mid, Reason: p.reason + " prune"}})
	}
}

// principalRef names a principal a step needs: an existing enabled one, or
// one this bundle creates, by address until the step runs.
func (p *planner) principalRef(addr, name string) (string, *uuid.UUID, bool) {
	cur, exists := p.st.People[name]
	_, declared := p.doc.Principals[name]
	switch {
	case exists && cur.Disabled:
		p.find(addr, KindUnresolvedReference, "principal %s is disabled", name)
		return "", nil, false
	case exists:
		id := cur.ID
		p.ref("principal", id)
		if !declared {
			p.find("principal."+name, KindUnmanagedReference, "%s names %s, which this bundle does not declare", addr, name)
		}
		return "", &id, true
	case declared:
		return "principal." + name, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no principal %q", name)
	return "", nil, false
}

// adminFloor blocks a plan that revokes an admin grant and would leave fewer
// than two approved admins on enabled human principals. PostgreSQL checks
// again when the stage commits.
func (p *planner) adminFloor() {
	if len(p.revokedAdmins) == 0 {
		return
	}
	n := 0
	for _, ps := range p.st.People {
		if g, ok := ps.Grants["admin"]; ok && g.Approved && ps.Kind == "human" && !ps.Disabled {
			n++
		}
	}
	if left := n + p.admins - len(p.revokedAdmins); left < 2 {
		p.find(p.revokedAdmins[0], KindAdminFloor, "this plan would leave %d admins; a tenant keeps at least two", left)
	}
}
```

- [ ] **Step 7: Run the tests**

Run: `go vet ./internal/bundle/ && go test -race -count=1 ./internal/bundle/`
Expected: PASS (new tests plus every Phase 20 test; the Phase 20 service tests still apply their bundles).

- [ ] **Step 8: Commit**

```bash
git add internal/bundle
git commit -m "feat(bundle): plan principals, roles, groups and members; the admin floor"
```

---

### Task 5: The policy, budget and price planners

**Files:**
- Create: `internal/bundle/plan_policy.go`, `internal/bundle/plan_budget.go`
- Modify: `internal/bundle/plan.go` (`diff`)
- Test: `internal/bundle/plan_governance_test.go`

**Interfaces:**
- Consumes: Task 4 state and payload fields; Task 3 `amount`, `cmpAmount`, `PolicyAddress`.
- Produces: `(*planner).policy()`, `(*planner).budgets()`, `(*planner).price(name string)`, `samePolicy(a, b json.RawMessage) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/bundle/plan_governance_test.go`:
```go
package bundle

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	polID   = uuid.MustParse("00000000-0000-4000-8000-00000000c001")
	rootID  = uuid.MustParse("00000000-0000-4000-8000-00000000c002")
	teamID  = uuid.MustParse("00000000-0000-4000-8000-00000000c003")
	priceID = uuid.MustParse("00000000-0000-4000-8000-00000000c004")
	openID  = uuid.MustParse("00000000-0000-4000-8000-00000000c005")
)

func TestAPolicyIsCreatedThenActivatedAndComparedCanonically(t *testing.T) {
	doc := `{"policy": {"content": ` + allowPolicy + `}}`
	st := empty()
	d := diff("gov", cs, mustDoc(t, doc), st, false)
	wantOps(t, d, "1 submit create "+PolicyAddress, "2 approve activate "+PolicyAddress)
	if d.Steps[1].Payload.ProposalStep != 1 || !slices.Contains(d.Refs, Ref{"policy", tenantID}) {
		t.Fatalf("steps %+v refs %+v", d.Steps, d.Refs)
	}
	id := polID
	st.Policy = PolicyState{ID: &id, Version: 3,
		Content: json.RawMessage(`{"rules":[{"reason":"permitted","verdict":"allow","id":"all"}],"format_version":1}`)}
	st.Managed[PolicyAddress] = polID
	wantOps(t, diff("gov", cs, mustDoc(t, doc), st, false))
	delete(st.Managed, PolicyAddress)
	wantFinding(t, diff("gov", cs, mustDoc(t, doc), st, false), PolicyAddress, KindUnmanaged)
	st.AddressElsewhere[PolicyAddress] = "other"
	wantFinding(t, diff("gov", cs, mustDoc(t, doc), st, false), PolicyAddress, KindUnmanaged)
}

const budgetDoc = `{"budgets": {
 "root": {"unit": "USD", "hard_limit": 1000},
 "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}}}`

func TestBudgetsAreCreatedParentsFirstAndRaisedByTheApprover(t *testing.T) {
	d := diff("money", cs, mustDoc(t, budgetDoc), empty(), false)
	wantOps(t, d,
		"1 submit create budget.root",
		"2 submit create budget.team",
		"3 submit propose budget.root",
		"4 submit propose budget.team",
		"5 submit set budget.team",
		"6 approve activate budget.root",
		"7 approve activate budget.team")
	if p := d.Steps[1].Payload; p.Parent != "budget.root" || p.Budget == nil || p.Budget.Unit != "USD" {
		t.Fatalf("create payload = %+v", p)
	}
	if p := d.Steps[2].Payload; p.Limit != "1000" || p.Parent != "budget.root" {
		t.Fatalf("propose payload = %+v", p)
	}
	if p := d.Steps[4].Payload; p.SoftLimit == nil || *p.SoftLimit != "150" || p.Limit != "" {
		t.Fatalf("soft limit payload = %+v", p)
	}
	if d.Steps[5].Payload.ProposalStep != 3 || d.Steps[6].Payload.ProposalStep != 4 {
		t.Fatalf("activations = %+v", d.Steps[5:])
	}
}

// moneyApplied is the state after budgetDoc was applied.
func moneyApplied() State {
	st := empty()
	soft, root := "150", rootID
	st.Budgets["root"] = BudgetState{ID: rootID, Unit: "USD", HardLimit: "1000"}
	st.Budgets["team"] = BudgetState{ID: teamID, Unit: "USD", ParentID: &root, HardLimit: "200", SoftLimit: &soft}
	st.Managed["budget.root"], st.Managed["budget.team"] = rootID, teamID
	return st
}

func TestBudgetLimitsAreComparedByValueAndLoweredChildrenFirst(t *testing.T) {
	raw := strings.Replace(budgetDoc, `"hard_limit": 1000`, `"hard_limit": 1000.000`, 1)
	wantOps(t, diff("money", cs, mustDoc(t, raw), moneyApplied(), false))
	raw = strings.Replace(budgetDoc, `"hard_limit": 1000`, `"hard_limit": 900`, 1)
	raw = strings.Replace(raw, `"hard_limit": 200`, `"hard_limit": 100`, 1)
	d := diff("money", cs, mustDoc(t, raw), moneyApplied(), false)
	wantOps(t, d, "1 submit set budget.team", "2 submit set budget.root")
	if p := d.Steps[0].Payload; p.Limit != "100" || p.ParentID == nil || *p.ParentID != teamID {
		t.Fatalf("decrease payload = %+v", p)
	}
}

func TestBudgetIdentityIsImmutableAndAnOpenIncreaseBlocks(t *testing.T) {
	raw := strings.Replace(budgetDoc, `"unit": "USD", "parent": "root"`, `"unit": "USD"`, 1)
	wantFinding(t, diff("money", cs, mustDoc(t, raw), moneyApplied(), false), "budget.team", KindUnsupported)
	st := moneyApplied()
	open := openID
	x := st.Budgets["root"]
	x.OpenProposal = &open
	st.Budgets["root"] = x
	wantFinding(t, diff("money", cs, mustDoc(t, budgetDoc), st, false), "budget.root", KindPending)
}

func TestAnAgentBudgetNamesItsAgent(t *testing.T) {
	st := empty()
	st.Agents["ledger-bot"] = AgentState{ID: agentID}
	raw := `{"budgets": {"bot": {"unit": "USD", "agent": "ledger-bot", "hard_limit": 0}}}`
	d := diff("money", cs, mustDoc(t, raw), st, false)
	wantOps(t, d, "1 submit create budget.bot")
	if p := d.Steps[0].Payload; p.OtherID == nil || *p.OtherID != agentID {
		t.Fatalf("create payload = %+v", p)
	}
	wantFinding(t, d, "agent.ledger-bot", KindUnmanagedReference)
}

const priceDoc = `{"prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD",
 "input_per_mtok": 2.5, "output_per_mtok": 10}}}`

func TestAPriceIsAddedWhenItDiffersFromTheOneInEffect(t *testing.T) {
	d := diff("rates", cs, mustDoc(t, priceDoc), empty(), false)
	wantOps(t, d, "1 submit create price.gpt")
	if p := d.Steps[0].Payload.Price; p == nil || p.InputPerMTok != "2.5" || p.OutputPerMTok != "10" ||
		p.CachedPerMTok != nil || p.EffectiveFrom != nil {
		t.Fatalf("price payload = %+v", p)
	}
	st := empty()
	st.Prices["openai gpt-4.1"] = PriceState{ID: priceID, Unit: "USD", Input: "2.50", Output: "10"}
	wantFinding(t, diff("rates", cs, mustDoc(t, priceDoc), st, false), "price.gpt", KindUnmanaged)
	st.Managed["price.gpt"] = priceID
	wantOps(t, diff("rates", cs, mustDoc(t, priceDoc), st, false))
	raw := strings.Replace(priceDoc, `"output_per_mtok": 10`, `"output_per_mtok": 12`, 1)
	wantOps(t, diff("rates", cs, mustDoc(t, raw), st, false), "1 submit create price.gpt")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/bundle/ -run 'Policy|Budget|Price'`
Expected: FAIL: the diffs have no steps (`steps:` empty, want `1 submit create policy.tenant`...).

- [ ] **Step 3: Write `plan_policy.go`**

```go
package bundle

import (
	"bytes"
	"encoding/json"

	"eacp/internal/governance"
)

// policy plans the tenant policy: a new version when the declared content
// differs from the active one (compared canonically), activated by the
// approver. One bundle per tenant owns it.
func (p *planner) policy() {
	if p.doc.Policy == nil {
		return
	}
	if other := p.st.AddressElsewhere[PolicyAddress]; other != "" {
		p.find(PolicyAddress, KindUnmanaged, "the tenant policy is managed by bundle %s", other)
		return
	}
	cur := p.st.Policy
	if cur.ID != nil {
		if _, managed := p.managed[PolicyAddress]; !managed {
			p.find(PolicyAddress, KindUnmanaged, "policy version %d (%s) is active and not managed by this bundle: add an import",
				cur.Version, *cur.ID)
			return
		}
		p.ref("policy", *cur.ID)
		if samePolicy(cur.Content, p.doc.Policy.Content) {
			return
		}
	} else {
		p.ref("policy", p.st.TenantID)
	}
	n := p.add(StageSubmit, PolicyAddress, OpCreate, Payload{Policy: p.doc.Policy.Content})
	p.add(StageApprove, PolicyAddress, OpActivate, Payload{ProposalStep: n, Reason: p.reason})
}

// samePolicy compares two policies as canonical JSON.
func samePolicy(a, b json.RawMessage) bool {
	ca, errA := governance.Canonicalize(a)
	cb, errB := governance.Canonicalize(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}
```

- [ ] **Step 4: Write `plan_budget.go`**

```go
package bundle

import (
	"slices"

	"github.com/google/uuid"

	"eacp/internal/budget"
	"eacp/internal/finops"
)

// budgets plans the declared accounts. Accounts are created parents first;
// limits are lowered children first at submit (a decrease needs one admin)
// and raised parents first at approval, so each child's escrow comes from a
// parent that already holds it; soft limits are set at submit.
func (p *planner) budgets() {
	type target struct {
		name, addr, par string
		parID          *uuid.UUID
		cur            BudgetState
	}
	var ts []target
	for _, name := range p.budgetOrder() {
		want, addr := p.doc.Budgets[name], "budget."+name
		cur, exists := p.st.Budgets[name]
		parentAddr, parentID, ok := p.budgetParent(addr, want)
		if !ok {
			continue
		}
		agentAddr, agentID, ok := p.budgetAgent(addr, want)
		if !ok {
			continue
		}
		if !exists {
			p.add(StageSubmit, addr, OpCreate, Payload{Budget: &budget.NewAccount{Name: name, Unit: want.Unit},
				Parent: parentAddr, ParentID: parentID, Other: agentAddr, OtherID: agentID})
			cur = BudgetState{HardLimit: "0"}
		} else {
			if !p.owned(addr, cur.ID) {
				continue
			}
			p.ref("budget", cur.ID)
			if cur.Unit != want.Unit {
				p.find(addr, KindUnsupported, "unit is immutable: %s in the registry, %s in the bundle", cur.Unit, want.Unit)
			}
			if parentAddr != "" || !sameID(cur.ParentID, parentID) {
				p.find(addr, KindUnsupported, "parent is immutable")
			}
			if agentAddr != "" || !sameID(cur.AgentID, agentID) {
				p.find(addr, KindUnsupported, "agent is immutable")
			}
			if cur.OpenProposal != nil {
				p.find(addr, KindPending, "limit change %s is open: apply or reject it through the API first",
					*cur.OpenProposal)
			}
		}
		par, parID := parent(addr, cur.ID)
		ts = append(ts, target{name: name, addr: addr, par: par, parID: parID, cur: cur})
	}
	for i := len(ts) - 1; i >= 0; i-- { // lowered children first
		t := ts[i]
		if limit, _ := amount(p.doc.Budgets[t.name].HardLimit); cmpAmount(limit, t.cur.HardLimit) < 0 {
			p.add(StageSubmit, t.addr, OpSet, Payload{Limit: limit, Reason: p.reason, Parent: t.par, ParentID: t.parID})
		}
	}
	for _, t := range ts { // raised parents first
		if limit, _ := amount(p.doc.Budgets[t.name].HardLimit); cmpAmount(limit, t.cur.HardLimit) > 0 {
			n := p.add(StageSubmit, t.addr, OpPropose, Payload{Limit: limit, Reason: p.reason, Parent: t.par,
				ParentID: t.parID})
			p.add(StageApprove, t.addr, OpActivate, Payload{ProposalStep: n, Reason: p.reason})
		}
	}
	for _, t := range ts {
		want := p.doc.Budgets[t.name].SoftLimit
		if want == nil {
			continue
		}
		soft, _ := amount(*want)
		if t.cur.SoftLimit != nil && cmpAmount(soft, *t.cur.SoftLimit) == 0 {
			continue
		}
		p.add(StageSubmit, t.addr, OpSet, Payload{SoftLimit: &soft, Reason: p.reason, Parent: t.par, ParentID: t.parID})
	}
}

// budgetOrder is the declared accounts, parents before their children.
func (p *planner) budgetOrder() []string {
	depth := func(name string) int {
		d := 0
		for cur := p.doc.Budgets[name].Parent; cur != "" && d <= len(p.doc.Budgets); cur = p.doc.Budgets[cur].Parent {
			if _, ok := p.doc.Budgets[cur]; !ok {
				break
			}
			d++
		}
		return d
	}
	names := sortedKeys(p.doc.Budgets)
	slices.SortStableFunc(names, func(a, b string) int { return depth(a) - depth(b) })
	return names
}

// budgetParent resolves an account's parent: an existing account, or one
// this bundle creates.
func (p *planner) budgetParent(addr string, want Budget) (string, *uuid.UUID, bool) {
	if want.Parent == "" {
		return "", nil, true
	}
	cur, exists := p.st.Budgets[want.Parent]
	_, declared := p.doc.Budgets[want.Parent]
	switch {
	case exists:
		if cur.Unit != want.Unit {
			p.find(addr, KindInvalid, "parent %s is in %s, not %s", want.Parent, cur.Unit, want.Unit)
			return "", nil, false
		}
		if cur.AgentID != nil {
			p.find(addr, KindInvalid, "parent %s belongs to an agent; an agent's account has no children", want.Parent)
			return "", nil, false
		}
		id := cur.ID
		p.ref("budget", id)
		if !declared {
			p.find("budget."+want.Parent, KindUnmanagedReference, "%s names %s, which this bundle does not declare",
				addr, want.Parent)
		}
		return "", &id, true
	case declared:
		return "budget." + want.Parent, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no budget %q", want.Parent)
	return "", nil, false
}

// budgetAgent resolves the agent an account is bound to.
func (p *planner) budgetAgent(addr string, want Budget) (string, *uuid.UUID, bool) {
	if want.Agent == "" {
		return "", nil, true
	}
	cur, exists := p.st.Agents[want.Agent]
	_, declared := p.doc.Agents[want.Agent]
	switch {
	case exists:
		id := cur.ID
		p.ref("agent", id)
		if !declared {
			p.find("agent."+want.Agent, KindUnmanagedReference, "%s names %s, which this bundle does not declare",
				addr, want.Agent)
		}
		return "", &id, true
	case declared:
		return "agent." + want.Agent, nil, true
	}
	p.find(addr, KindUnresolvedReference, "no agent %q", want.Agent)
	return "", nil, false
}

// price plans a declared price: a new one, effective when it is applied,
// whenever the declared rates differ from the price in effect.
func (p *planner) price(name string) {
	want, addr := p.doc.Prices[name], "price."+name
	cur, exists := p.st.Prices[want.Provider+" "+want.Model]
	if exists {
		if _, managed := p.managed[addr]; !managed {
			if other := p.st.ManagedElsewhere[cur.ID]; other != "" {
				p.find(addr, KindUnmanaged, "managed by bundle %s", other)
			} else {
				p.find(addr, KindUnmanaged, "%s %s is priced (%s) and not managed by this bundle: add an import",
					want.Provider, want.Model, cur.ID)
			}
			return
		}
		p.ref("price", cur.ID)
		if samePrice(cur, want) {
			return
		}
	}
	np := finops.NewPrice{Provider: want.Provider, Model: want.Model, Unit: want.Unit, Reason: p.reason}
	np.InputPerMTok, _ = amount(want.InputPerMTok)
	np.OutputPerMTok, _ = amount(want.OutputPerMTok)
	if want.CachedPerMTok != nil {
		c, _ := amount(*want.CachedPerMTok)
		np.CachedPerMTok = &c
	}
	p.add(StageSubmit, addr, OpCreate, Payload{Price: &np})
}

// samePrice compares a price in effect with a declared one by value. An
// absent cached-input price is the input price.
func samePrice(cur PriceState, want Price) bool {
	in, _ := amount(want.InputPerMTok)
	out, _ := amount(want.OutputPerMTok)
	cached, curCached := in, cur.Input
	if want.CachedPerMTok != nil {
		cached, _ = amount(*want.CachedPerMTok)
	}
	if cur.Cached != nil {
		curCached = *cur.Cached
	}
	return cur.Unit == want.Unit && cmpAmount(cur.Input, in) == 0 && cmpAmount(cur.Output, out) == 0 &&
		cmpAmount(curCached, cached) == 0
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
```

- [ ] **Step 5: Call them from `diff`**

In `plan.go`'s `diff`, after the agents loop and before `p.orphans()`:
```go
	p.policy()
	p.budgets()
	for _, name := range sortedKeys(doc.Prices) {
		p.price(name)
	}
```

- [ ] **Step 6: Run the tests**

Run: `go vet ./internal/bundle/ && go test -race -count=1 ./internal/bundle/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/bundle
git commit -m "feat(bundle): plan the tenant policy, budget limits and prices"
```

---

### Task 6: Execute the new steps; the service end to end

**Files:**
- Create: `internal/bundle/execute_governance.go`
- Modify: `internal/bundle/execute.go`, `internal/bundle/service.go`
- Test: `internal/bundle/service_governance_test.go`

**Interfaces:**
- Consumes: Task 1 `Tx` types; Task 4/5 steps and payloads; Task 2 `admin_floor` hint.
- Produces: `applyGovernance(ctx, tx pgx.Tx, st Step, ids map[string]uuid.UUID, produced map[int]uuid.UUID) (uuid.UUID, bool, error)`; `managedKinds`; `classify` maps the admin floor to `step_failed`.

- [ ] **Step 1: Write the failing tests**

`internal/bundle/service_governance_test.go`:
```go
package bundle_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"eacp/internal/bundle"
	"eacp/internal/governance"
	"eacp/internal/registry"
	"eacp/internal/registry/registrytest"
	"eacp/internal/storage"
)

const peopleDoc = `{
 "principals": {"dana": {"kind": "human", "subject": "dana@example.com", "display_name": "Dana", "roles": ["auditor"]}},
 "groups": {"ops": {"display_name": "Ops", "schedule_weight": 2, "members": ["dana"]}}}`

func gov(name, doc string) bundle.Request {
	return bundle.Request{Bundle: name, Desired: json.RawMessage(doc)}
}

// declare is an existing fixture principal as a bundle declares it.
func declare(t *testing.T, f *registrytest.Fixture, name string, roles ...string) string {
	t.Helper()
	var kind, display string
	var subject *string
	ok(t, storage.InTenantTx(context.Background(), f.Owner, f.Tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT kind, subject, display_name FROM eacp.principals
			WHERE name = $1`, name).Scan(&kind, &subject, &display)
	}))
	p := map[string]any{"kind": kind, "display_name": display, "roles": append([]string{}, roles...)}
	if subject != nil {
		p["subject"] = *subject
	}
	raw, err := json.Marshal(p)
	ok(t, err)
	return fmt.Sprintf("%q: %s", name, raw)
}

// adopt is the imports block that adopts existing fixture principals.
func adopt(f *registrytest.Fixture, names ...string) string {
	var out []string
	for _, n := range names {
		out = append(out, fmt.Sprintf(`{"to": "principal.%s", "id": "%s"}`, n, f.P[n]))
	}
	return `"imports": [` + strings.Join(out, ", ") + `]`
}

// submitted plans r and submits it as who.
func submitted(t *testing.T, f *registrytest.Fixture, s *bundle.Service, who string, r bundle.Request) bundle.ChangeSet {
	t.Helper()
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, who), r)
	ok(t, err)
	cs, err := s.Submit(ctx, as(f, who), p.ID)
	ok(t, err)
	return cs
}

func TestAnIdentityBundleIsAppliedByTwoAdmins(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	p, err := s.Plan(ctx, as(f, "alice"), gov("people", peopleDoc))
	ok(t, err)
	var got []string
	for _, st := range p.Steps {
		got = append(got, st.Stage+" "+st.Op+" "+st.Address)
	}
	if want := "submit create principal.dana|submit propose role.dana.auditor|submit create group.ops|" +
		"submit create member.ops.dana|approve activate role.dana.auditor"; strings.Join(got, "|") != want {
		t.Fatalf("steps = %v", got)
	}
	_, err = s.Submit(ctx, as(f, "alice"), p.ID)
	ok(t, err)
	_, err = s.Approve(ctx, as(f, "alice"), p.ID)
	wantCode(t, err, bundle.CodeSamePrincipal)
	// A registry approver may approve change sets, but the grant trigger
	// wants an admin.
	_, err = s.Approve(ctx, as(f, "rita"), p.ID)
	if wantCode(t, err, bundle.CodeStepFailed); !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("a registry approver approving a grant: %v", err)
	}
	cs, err := s.Approve(ctx, as(f, "bob"), p.ID)
	ok(t, err)
	if cs.State != "APPLIED" {
		t.Fatalf("state = %s", cs.State)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants g JOIN eacp.principals p ON p.id = g.principal_id
		WHERE p.name = 'dana' AND g.role = 'auditor' AND g.proposed_by = $1 AND g.approved_by = $2`,
		f.P["alice"], f.P["bob"]); n != 1 {
		t.Fatal("dana's grant was not proposed by alice and approved by bob")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.group_memberships m JOIN eacp.groups g ON g.id = m.group_id
		WHERE g.name = 'ops' AND g.schedule_weight = 2 AND m.removed_at IS NULL`); n != 1 {
		t.Fatal("the membership was not added")
	}
	if auditCount(t, f, "change_set.submitted") != 1 || auditCount(t, f, "change_set.applied") != 1 {
		t.Fatal("the change set's moves are journaled")
	}
	again, err := s.Plan(ctx, as(f, "alice"), gov("people", peopleDoc))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
}

func TestTheGranteeCannotApproveTheirOwnGrant(t *testing.T) {
	f, s := setup(t)
	doc := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin", "auditor") +
		`}, ` + adopt(f, "alice", "bob") + `}`
	cs := submitted(t, f, s, "alice", gov("admins", doc))
	_, err := s.Approve(context.Background(), as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStepFailed)
}

func TestTheAdminFloorHoldsWhenAPlanRevokesAnAdmin(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	both := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin") + `}, ` +
		adopt(f, "alice", "bob") + `}`
	if cs := submitted(t, f, s, "alice", gov("admins", both)); cs.State != "APPLIED" {
		t.Fatalf("adoption = %s", cs.State)
	}
	lone := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+`}}`)
	lone.Prune = true
	_, err := s.Plan(ctx, as(f, "alice"), lone)
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if !strings.Contains(fmt.Sprint(be.Findings), bundle.KindAdminFloor) {
		t.Fatalf("findings = %+v", be.Findings)
	}
	withDave := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+
		`, "dave": {"kind": "human", "subject": "dave@example.com", "display_name": "Dave", "roles": ["admin"]}}}`)
	withDave.Prune = true
	cs := submitted(t, f, s, "alice", withDave)
	cs, err = s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants g JOIN eacp.principals p ON p.id = g.principal_id
		WHERE g.role = 'admin' AND g.approved_at IS NOT NULL AND g.revoked_at IS NULL AND p.name IN ('bob', 'dave')`); n != 2 ||
		count(t, f, `SELECT count(*) FROM eacp.role_grants WHERE role = 'admin' AND revoked_at IS NULL
			AND principal_id = $1`, f.P["alice"]) != 0 {
		t.Fatal("bob and dave are the admins, alice is not")
	}
}

func TestAConcurrentRevokeCannotBreakTheAdminFloor(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	// carol is a third admin that the bundle does not manage.
	carolAdmin := f.ID(t, "alice", `INSERT INTO eacp.role_grants (tenant_id, principal_id, role)
		VALUES (eacp.current_tenant_id(), $1, 'admin') RETURNING id`, f.P["carol"])
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, carolAdmin))
	both := `{"principals": {` + declare(t, f, "alice", "admin") + `, ` + declare(t, f, "bob", "admin") + `}, ` +
		adopt(f, "alice", "bob") + `}`
	submitted(t, f, s, "alice", gov("admins", both))
	lone := gov("admins", `{"principals": {`+declare(t, f, "alice")+`, `+declare(t, f, "bob", "admin")+`}}`)
	lone.Prune = true
	cs := submitted(t, f, s, "alice", lone) // three admins, one revoked: two remain
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET revoked_at = now(), revoke_reason = 'left' WHERE id = $1`, carolAdmin))
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStepFailed)
	if n := count(t, f, `SELECT count(*) FROM eacp.role_grants WHERE role = 'admin' AND revoked_at IS NULL
		AND principal_id = $1`, f.P["alice"]); n != 1 {
		t.Fatal("the approval committed below two admins")
	}
}

func TestAPolicyIsCreatedByOneAdminAndActivatedByAnother(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"policy": {"content": ` + allowPolicy + `}}`
	cs := submitted(t, f, s, "alice", gov("policy", doc))
	cs, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	cur, err := governance.NewStore(f.App).CurrentPolicy(ctx, f.Tenant)
	ok(t, err)
	if cs.Steps[0].ObjectID == nil || cur.ID != *cs.Steps[0].ObjectID {
		t.Fatalf("current policy %s, created %v", cur.ID, cs.Steps[0].ObjectID)
	}
	reformatted := `{"policy": {"content": {"rules": [{"reason": "permitted", "verdict": "allow", "id": "all"}],
	 "format_version": 1}}}`
	again, err := s.Plan(ctx, as(f, "alice"), gov("policy", reformatted))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
	_, err = s.Plan(ctx, as(f, "alice"), gov("other", doc))
	be := wantCode(t, err, bundle.CodePlanBlocked)
	if len(be.Findings) != 1 || be.Findings[0].Address != bundle.PolicyAddress || be.Findings[0].Kind != bundle.KindUnmanaged {
		t.Fatalf("findings = %+v", be.Findings)
	}
}

func TestABudgetIsCreatedAndRaisedInOneChangeSet(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"budgets": {"root": {"unit": "USD", "hard_limit": 1000},
	 "team": {"unit": "USD", "parent": "root", "hard_limit": 200, "soft_limit": 150}}}`
	cs := submitted(t, f, s, "alice", gov("money", doc))
	if len(cs.Steps) != 7 {
		t.Fatalf("steps = %+v", cs.Steps)
	}
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_accounts
		WHERE (name, hard_limit) IN (('root', 1000), ('team', 200))`); n != 2 {
		t.Fatal("the limits were not raised")
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_soft_limits l JOIN eacp.budget_accounts b ON b.id = l.account_id
		WHERE b.name = 'team' AND l.monthly_limit = 150`); n != 1 {
		t.Fatal("the soft limit was not set")
	}
	again, err := s.Plan(ctx, as(f, "alice"), gov("money", doc))
	ok(t, err)
	if len(again.Steps) != 0 {
		t.Fatalf("replan = %+v", again.Steps)
	}
	lower := strings.NewReplacer(`"hard_limit": 1000`, `"hard_limit": 900`, `"hard_limit": 200`, `"hard_limit": 100`).Replace(doc)
	if cs := submitted(t, f, s, "alice", gov("money", lower)); cs.State != "APPLIED" {
		t.Fatalf("decreases need one admin: %s", cs.State)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.budget_accounts
		WHERE (name, hard_limit) IN (('root', 900), ('team', 100))`); n != 2 {
		t.Fatal("the limits were not lowered")
	}
}

func TestAPriceIsAddedForwardOnly(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	doc := `{"prices": {"gpt": {"provider": "openai", "model": "gpt-4.1", "unit": "USD",
	 "input_per_mtok": 2.5, "output_per_mtok": 10}}}`
	if cs := submitted(t, f, s, "alice", gov("rates", doc)); cs.State != "APPLIED" {
		t.Fatalf("a price needs one admin: %s", cs.State)
	}
	same, err := s.Plan(ctx, as(f, "alice"), gov("rates", strings.Replace(doc, "2.5", "2.50", 1)))
	ok(t, err)
	if len(same.Steps) != 0 {
		t.Fatalf("replan = %+v", same.Steps)
	}
	submitted(t, f, s, "alice", gov("rates", strings.Replace(doc, `"output_per_mtok": 10`, `"output_per_mtok": 12`, 1)))
	if n := count(t, f, `SELECT count(*) FROM eacp.model_prices WHERE provider = 'openai' AND model = 'gpt-4.1'`); n != 2 {
		t.Fatalf("prices = %d, want the old one kept and a new one added", n)
	}
	if n := count(t, f, `SELECT count(*) FROM eacp.model_prices a JOIN eacp.model_prices b
		ON b.model = a.model AND b.output_per_mtok = 10 WHERE a.output_per_mtok = 12 AND a.effective_from > b.effective_from`); n != 1 {
		t.Fatal("the new price takes effect after the old one")
	}
}

func TestAConcurrentGrantApprovalMakesTheChangeSetStale(t *testing.T) {
	f, s := setup(t)
	cs := submitted(t, f, s, "alice", gov("people", peopleDoc))
	grant := cs.Steps[1].ObjectID
	if grant == nil {
		t.Fatalf("steps = %+v", cs.Steps)
	}
	ok(t, f.Exec("bob", `UPDATE eacp.role_grants SET approved_at = now() WHERE id = $1`, *grant))
	_, err := s.Approve(context.Background(), as(f, "bob"), cs.ID)
	wantCode(t, err, bundle.CodeStale)
}

func TestIdentityDrift(t *testing.T) {
	f, s := setup(t)
	ctx := context.Background()
	cs := submitted(t, f, s, "alice", gov("people", peopleDoc))
	_, err := s.Approve(ctx, as(f, "bob"), cs.ID)
	ok(t, err)
	ok(t, f.Exec("alice", `UPDATE eacp.group_memberships SET removed_at = now(), remove_reason = 'left'
		WHERE principal_id = (SELECT id FROM eacp.principals WHERE name = 'dana')`))
	dr, err := s.Drift(ctx, as(f, "audra"), "people")
	ok(t, err)
	got := map[string]string{}
	for _, e := range dr.Entries {
		got[e.Address] = e.Status
	}
	if got["principal.dana"] != bundle.DriftInSync || got["group.ops"] != bundle.DriftInSync ||
		got["member.ops.dana"] != bundle.DriftModified {
		t.Fatalf("drift = %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -count=1 ./internal/bundle/ -run 'IdentityBundle|Grantee|AdminFloorHolds|ConcurrentRevoke|PolicyIsCreatedBy|BudgetIsCreated|PriceIsAdded|ConcurrentGrant|IdentityDrift'`
Expected: FAIL: `step_failed … unknown step create principal.dana` on submit.

- [ ] **Step 3: Write `execute_governance.go`**

```go
package bundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"eacp/internal/budget"
	"eacp/internal/finops"
	"eacp/internal/governance"
	"eacp/internal/registry"
)

// applyGovernance makes one identity, policy, budget or price step's write
// through the same Tx the API uses. It reports false for any other step.
func applyGovernance(ctx context.Context, tx pgx.Tx, st Step, ids map[string]uuid.UUID,
	produced map[int]uuid.UUID) (uuid.UUID, bool, error) {
	p := st.Payload
	ref := func(addr string, id *uuid.UUID) (uuid.UUID, error) {
		if id != nil {
			return *id, nil
		}
		if x, ok := ids[addr]; ok && addr != "" {
			return x, nil
		}
		return uuid.Nil, fmt.Errorf("%q was not created by this change set", addr)
	}
	optional := func(addr string, id *uuid.UUID) (*uuid.UUID, error) {
		if id == nil && addr == "" {
			return nil, nil
		}
		x, err := ref(addr, id)
		return &x, err
	}
	proposal := func() (uuid.UUID, error) {
		if id, ok := produced[p.ProposalStep]; ok {
			return id, nil
		}
		return uuid.Nil, fmt.Errorf("proposal step %d did not run", p.ProposalStep)
	}
	rtx, btx, ftx, gtx := registry.Tx{Tx: tx}, budget.Tx{Tx: tx}, finops.Tx{Tx: tx}, governance.Tx{Tx: tx}
	kind, _, _ := strings.Cut(st.Address, ".")
	switch st.Op + " " + kind {
	case "create principal":
		if p.Principal == nil {
			return uuid.Nil, true, errPayload
		}
		pr, err := rtx.CreatePrincipal(ctx, *p.Principal)
		return pr.ID, true, err
	case "create group":
		if p.Group == nil {
			return uuid.Nil, true, errPayload
		}
		id, err := rtx.CreateGroup(ctx, p.Group.Name, p.Group.DisplayName, p.Group.Weight)
		return id, true, err
	case "create member":
		group, err := ref(p.Parent, p.ParentID)
		if err != nil {
			return uuid.Nil, true, err
		}
		principal, err := ref(p.Other, p.OtherID)
		if err != nil {
			return uuid.Nil, true, err
		}
		id, err := rtx.AddMember(ctx, group, principal)
		return id, true, err
	case "propose role":
		principal, err := ref(p.Parent, p.ParentID)
		if err != nil {
			return uuid.Nil, true, err
		}
		id, err := rtx.ProposeRole(ctx, principal, p.Name)
		return id, true, err
	case "activate role":
		grant, err := proposal()
		if err != nil {
			return uuid.Nil, true, err
		}
		return grant, true, rtx.ApproveRole(ctx, grant)
	case "revoke role", "revoke member":
		if p.ObjectID == nil {
			return uuid.Nil, true, errPayload
		}
		if kind == "role" {
			return *p.ObjectID, true, rtx.RevokeRole(ctx, *p.ObjectID, p.Reason)
		}
		return *p.ObjectID, true, rtx.RemoveMember(ctx, *p.ObjectID, p.Reason)
	case "create policy":
		if len(p.Policy) == 0 {
			return uuid.Nil, true, errPayload
		}
		pol, err := gtx.CreatePolicy(ctx, p.Policy)
		return pol.ID, true, err
	case "activate policy":
		pol, err := proposal()
		if err != nil {
			return uuid.Nil, true, err
		}
		return pol, true, gtx.ActivatePolicy(ctx, pol, p.Reason)
	case "create budget":
		if p.Budget == nil {
			return uuid.Nil, true, errPayload
		}
		n := *p.Budget
		var err error
		if n.ParentID, err = optional(p.Parent, p.ParentID); err != nil {
			return uuid.Nil, true, err
		}
		if n.AgentID, err = optional(p.Other, p.OtherID); err != nil {
			return uuid.Nil, true, err
		}
		id, err := btx.CreateAccount(ctx, n)
		return id, true, err
	case "set budget":
		acct, err := ref(p.Parent, p.ParentID)
		if err != nil {
			return uuid.Nil, true, err
		}
		if p.SoftLimit != nil {
			return acct, true, ftx.SetSoftLimit(ctx, acct, p.SoftLimit, p.Reason)
		}
		c, err := btx.ChangeLimit(ctx, acct, p.Limit, p.Reason)
		if err == nil && c.State != "APPLIED" {
			err = &registry.Error{Kind: registry.ErrConflict, Msg: "the limit is no longer above " + p.Limit}
		}
		return acct, true, err
	case "propose budget":
		acct, err := ref(p.Parent, p.ParentID)
		if err != nil {
			return uuid.Nil, true, err
		}
		c, err := btx.ChangeLimit(ctx, acct, p.Limit, p.Reason)
		if err == nil && c.State != "PROPOSED" {
			err = &registry.Error{Kind: registry.ErrConflict, Msg: "the limit is no longer below " + p.Limit}
		}
		return c.ID, true, err
	case "activate budget":
		change, err := proposal()
		if err != nil {
			return uuid.Nil, true, err
		}
		_, err = btx.DecideLimitChange(ctx, change, true, p.Reason)
		return change, true, err
	case "create price":
		if p.Price == nil {
			return uuid.Nil, true, errPayload
		}
		pr, err := ftx.AddPrice(ctx, *p.Price)
		return pr.ID, true, err
	}
	return uuid.Nil, false, nil
}
```

- [ ] **Step 4: Route steps to it in `execute.go`**

Add below `errPayload`:
```go
// managedKinds are the kinds whose created or imported objects a bundle
// manages (bundle_resources).
var managedKinds = map[string]bool{"connector": true, "tool": true, "agent": true, "version": true,
	"principal": true, "group": true, "policy": true, "budget": true, "price": true}
```
In `run`, drop `rtx := registry.Tx{Tx: tx}`, call `apply(ctx, tx, st, ids, produced)`, and replace the kind test:
```go
		if kind, _, _ := strings.Cut(st.Address, "."); managedKinds[kind] {
```
Change `apply` to take the transaction, resolve an owner created by this change set, and fall through to `applyGovernance`:
```go
func apply(ctx context.Context, tx pgx.Tx, st Step, ids map[string]uuid.UUID,
	produced map[int]uuid.UUID) (uuid.UUID, error) {
	rtx := registry.Tx{Tx: tx}
	p := st.Payload
```
```go
	case "create agent":
		if p.Agent == nil {
			return uuid.Nil, errPayload
		}
		na := *p.Agent
		if p.Other != "" {
			id, ok := ids[p.Other]
			if !ok {
				return uuid.Nil, fmt.Errorf("owner %q was not created by this change set", p.Other)
			}
			if strings.HasPrefix(p.Other, "group.") {
				na.OwnerGroupID = id
			} else {
				na.OwnerPrincipalID = id
			}
		}
		ag, err := rtx.RegisterAgent(ctx, na)
		return ag.ID, err
```
and before the `if st.Op == OpImport` fallback at the end of `apply`:
```go
	if id, ok, err := applyGovernance(ctx, tx, st, ids, produced); ok {
		return id, err
	}
```

- [ ] **Step 5: Map the admin floor in `service.go`**

In `classify`, after the `40001`/`40P01` case:
```go
	if errors.As(err, &p) && p.Code == "23514" && p.Hint == "admin_floor" {
		return &Error{Code: CodeStepFailed, Msg: p.Message + ": a tenant keeps at least two admins"}
	}
```

- [ ] **Step 6: Run the tests**

Run: `go vet ./internal/bundle/ && go test -race -count=1 ./internal/bundle/ ./internal/api/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/bundle
git commit -m "feat(bundle): execute identity, policy, budget and price steps through the domain Tx types"
```

---

### Task 7: API and eacpctl

**Files:**
- Modify: `cmd/eacpctl/bundle.go`
- Test: `internal/api/bundle_test.go`, `cmd/eacpctl/bundle_test.go`

**Interfaces:**
- Consumes: Task 6 (the API needs no change: `/v1/change-sets` carries the larger document).
- Produces: eacpctl reads `resources.{principals, groups, budgets, prices, policy.file}`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/bundle_test.go` (it pins the API layer; the behaviour's RED was Task 6, so this one may pass at once):
```go
func TestAnIdentityBundleThroughTheAPI(t *testing.T) {
	h := newHarness(t)
	doc := `{"principals": {"dana": {"kind": "human", "subject": "dana@example.com", "display_name": "Dana",
	  "roles": ["auditor"]}},
	 "budgets": {"ops": {"unit": "USD", "hard_limit": 50}}}`
	code, body := h.as("alice", "POST", "/v1/change-sets", map[string]any{"bundle": "people", "desired": json.RawMessage(doc)})
	h.want(201, code, body)
	id, _ := body["id"].(string)
	code, body = h.as("alice", "POST", "/v1/change-sets/"+id+"/submit", nil)
	h.want(200, code, body)
	code, body = h.as("alice", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(403, code, body)
	code, body = h.as("bob", "POST", "/v1/change-sets/"+id+"/approve", nil)
	h.want(200, code, body)
	if body["state"] != "APPLIED" {
		t.Fatalf("approve = %v", body)
	}
	code, body = h.as("audra", "GET", "/v1/bundles/people/drift", nil)
	h.want(200, code, body)
	if entries, _ := body["entries"].([]any); len(entries) != 2 {
		t.Fatalf("drift = %v", body)
	}
}
```

Append to `cmd/eacpctl/bundle_test.go`:
```go
func TestBundleGovernanceSectionsAndPolicyFile(t *testing.T) {
	const yml = `bundle:
  name: people
resources:
  principals:
    dana: {kind: human, subject: dana@example.com, display_name: Dana, roles: [auditor]}
  policy: {file: policies/tenant.json}
  budgets:
    ops: {unit: USD, hard_limit: 50}
`
	const policy = `{"format_version": 1, "rules": [{"id": "all", "verdict": "allow", "reason": "permitted"}]}`
	dir := writeBundle(t, map[string]string{"eacp.yml": yml, "policies/tenant.json": policy,
		"resources/more.yml": "resources:\n  groups:\n    ops: {display_name: Ops, members: [dana]}\n" +
			"  prices:\n    gpt: {provider: openai, model: gpt-4.1, unit: USD, input_per_mtok: 2.5, output_per_mtok: 10}\n"})
	b, err := loadBundle(dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"principals", "groups", "budgets", "prices"} {
		if _, ok := b.desired[k]; !ok {
			t.Fatalf("no %s in %v", k, b.desired)
		}
	}
	pol, _ := b.desired["policy"].(map[string]any)
	content, _ := pol["content"].(map[string]any)
	if content["format_version"] != json.Number("1") {
		t.Fatalf("policy = %v", b.desired["policy"])
	}
	for name, file := range map[string]string{"outside": "../tenant.json",
		"absolute": filepath.Join(dir, "policies", "tenant.json")} {
		bad := writeBundle(t, map[string]string{"eacp.yml": strings.Replace(yml, "policies/tenant.json", file, 1)})
		if _, err := loadBundle(bad, "", nil); err == nil || !strings.Contains(err.Error(), "policy") {
			t.Errorf("%s: %v", name, err)
		}
	}
	twice := writeBundle(t, map[string]string{"eacp.yml": yml, "policies/tenant.json": policy,
		"resources/p.yml": "resources:\n  policy: {file: policies/tenant.json}\n"})
	if _, err := loadBundle(twice, "", nil); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("a policy declared twice: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify the CLI one fails**

Run: `go test -count=1 ./cmd/eacpctl/ -run TestBundleGovernanceSectionsAndPolicyFile && go test -count=1 ./internal/api/ -run TestAnIdentityBundleThroughTheAPI`
Expected: the CLI test FAILS (`eacp.yml: … field principals not found in type main.bundleResources`); the API test PASSES.

- [ ] **Step 3: Implement the sections in `cmd/eacpctl/bundle.go`**

Add `"bytes"` to the imports. Replace `bundleResources`:
```go
type bundleResources struct {
	Principals map[string]any `yaml:"principals"`
	Groups     map[string]any `yaml:"groups"`
	Connectors map[string]any `yaml:"connectors"`
	Agents     map[string]any `yaml:"agents"`
	Budgets    map[string]any `yaml:"budgets"`
	Prices     map[string]any `yaml:"prices"`
	Policy     *bundlePolicy  `yaml:"policy"`
}

// bundlePolicy names the tenant policy's JSON file, relative to the bundle
// directory.
type bundlePolicy struct {
	File string `yaml:"file"`
}

type section struct {
	key string
	m   *map[string]any
}

// sections are the mergeable resources: blocks, by their document key.
func (r *bundleResources) sections() []section {
	return []section{{"principals", &r.Principals}, {"groups", &r.Groups}, {"connectors", &r.Connectors},
		{"agents", &r.Agents}, {"budgets", &r.Budgets}, {"prices", &r.Prices}}
}
```
In `loadBundle`, replace everything from `res := bundleResources{...}` through the loop over `resources/*.yml` with:
```go
	var res bundleResources
	var policy *bundlePolicy
	merge := func(src *bundleResources, file string) error {
		dst := res.sections()
		for i, s := range src.sections() {
			if *dst[i].m == nil {
				*dst[i].m = map[string]any{}
			}
			for k, v := range *s.m {
				if _, dup := (*dst[i].m)[k]; dup {
					return fmt.Errorf("%s: %s %q is declared twice", file, strings.TrimSuffix(s.key, "s"), k)
				}
				(*dst[i].m)[k] = v
			}
		}
		if src.Policy != nil {
			if policy != nil {
				return fmt.Errorf("%s: the policy is declared twice", file)
			}
			policy = src.Policy
		}
		return nil
	}
	if err := merge(&cfg.Resources, "eacp.yml"); err != nil {
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
		if err := merge(&rf.Resources, f); err != nil {
			return loadedBundle{}, err
		}
	}
```
and replace the `desired` construction before `imports` with:
```go
	desired := map[string]any{}
	for _, s := range res.sections() {
		if len(*s.m) > 0 {
			desired[s.key] = *s.m
		}
	}
	if policy != nil {
		content, err := readPolicy(dir, policy.File)
		if err != nil {
			return b, err
		}
		desired["policy"] = map[string]any{"content": content}
	}
```
Add:
```go
// readPolicy reads the policy file as JSON with its numbers intact. The file
// stays inside the bundle directory and under 1 MiB.
func readPolicy(dir, file string) (any, error) {
	if file == "" || filepath.IsAbs(file) {
		return nil, fmt.Errorf("policy.file %q: give a path relative to the bundle directory", file)
	}
	path := filepath.Join(dir, file)
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("policy.file %q: stays inside the bundle directory", file)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("policy.file: %v", err)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("policy.file %q: over 1 MiB", file)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy.file: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("policy.file %q: %v", file, err)
	}
	return v, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go vet ./cmd/eacpctl/ && go test -race -count=1 ./cmd/eacpctl/ ./internal/api/`
Expected: PASS (including every Phase 20 CLI test, whose duplicate-resource message still names `agent "buyer"`).

- [ ] **Step 5: Commit**

```bash
git add cmd/eacpctl/bundle.go cmd/eacpctl/bundle_test.go internal/api/bundle_test.go
git commit -m "feat(eacpctl): principals, groups, budgets, prices and the policy file in bundles"
```

---

### Task 8: Demo, documentation and the full suite

**Files:**
- Modify: `test/demo/slice_c_test.go`, `docs/DEMO.md`, `docs/adr/ADR-026-governance-as-code.md`, `docs/adr/README.md`, `docs/MASTER_PLAN.md`, `docs/INVARIANTS.md`, `AGENTS.md`

- [ ] **Step 1: Extend the demo step**

At the end of `governanceAsCode` in `test/demo/slice_c_test.go`:
```go
	// Phase 21: people and money as code. An admin plans and submits; a
	// second admin approves the grant and the budget increase.
	people := json.RawMessage(`{
	 "principals": {"dana": {"kind": "human", "subject": "dana@globex.example", "display_name": "Dana",
	   "roles": ["auditor"]}},
	 "budgets": {"ledger": {"unit": "USD", "hard_limit": 500, "soft_limit": 400}}}`)
	plan = d.must(201, "alice", "POST", "/v1/change-sets", map[string]any{"bundle": "people", "desired": people})
	id = plan["id"].(string)
	d.must(200, "alice", "POST", "/v1/change-sets/"+id+"/submit", nil)
	d.must(403, "alice", "POST", "/v1/change-sets/"+id+"/approve", nil)
	applied = d.must(200, "bob", "POST", "/v1/change-sets/"+id+"/approve", nil)
	d.logf("people bundle: alice granted dana auditor and funded ledger (500 USD, soft 400); bob approved: %s",
		applied["state"])
```
Update its step title in `TestSliceCDemo` to `C7. Governance-as-Code: registry (erin, rita), people and budgets (alice, bob), drift` and the doc comment of `governanceAsCode`; update the C7 row of `docs/DEMO.md` the same way.

- [ ] **Step 2: Run the demo**

Run (background, Git Bash): `MSYS_NO_PATHCONV=1 DEMO=C scripts/demo.sh > <workspace>/demo-c.log 2>&1; echo "exit $?" >> <workspace>/demo-c.log`
Expected: `--- PASS: TestSliceCDemo` and `exit 0`.

- [ ] **Step 3: Update the documents**

- `docs/adr/ADR-026-governance-as-code.md`: status `Accepted (Rev 1.1)`; add a section `## Revision 1.1 (Phase 21): identity, policy, budgets and prices` stating: the new sections and addresses (`principal.x`, `group.x`, `member.g.p`, `role.p.r`, `policy.tenant`, `budget.x`, `price.x`); the submit and approve order; every step through `registry.Tx`, `governance.Tx`, `budget.Tx`, `finops.Tx`; grants and memberships of a managed principal or group are managed with it; prune revokes grants and removes memberships only; `pending` blocks for an open grant or limit change; the two-admin floor at plan time and in `change_sets_commit` (HINT `admin_floor`); budget refs exclude counters; one bundle owns the policy; prices are added effective at apply, never removed or backdated; a principal is never disabled or enabled; the target of a declared owner, member, parent or agent may be created by the same change set (`Other`/`OtherID`).
- `docs/adr/README.md`: the ADR-026 row becomes `Accepted (Rev 1.1) | Phase 20–21`.
- `docs/MASTER_PLAN.md` §93b: add `> **Status (2026-09-25): delivered.** …` summarising the above in two sentences.
- `docs/INVARIANTS.md`: after the Phase 20 paragraph, a Phase 21 paragraph (identity, policy, budgets, prices; the admin floor; `schema_phase21_test.go`, `plan_identity_test.go`, `plan_governance_test.go`, `service_governance_test.go`). Under `## 3` add ``- `internal/bundle` TestABudgetIsCreatedAndRaisedInOneChangeSet — a bundle's limit increase is proposed by one admin, escrowed from its parent and applied by a second``. Under `## 17` add ``- `internal/bundle` TestAnIdentityBundleIsAppliedByTwoAdmins — a bundle's grant is proposed by the submitter and approved by a second admin; the change set's moves are journaled``.
- `AGENTS.md`: status line adds `Phase 21 (Governance-as-Code for identity, policy, budgets and prices, ADR-026 Rev 1.1) is complete.` (and drops "awaits go-ahead"); extend the Governance-as-Code bullet with: `Identity, policy, budget and price steps run through registry.Tx, governance.Tx, budget.Tx and finops.Tx. A bundle never disables or enables a principal, approves someone else's proposal (pending blocks), clears a soft limit or backdates a price; prune revokes grants and removes memberships only, never below two admins (planner and change_sets_commit).`

- [ ] **Step 4: Run the full suite**

Run: `go vet ./... && go test -race -count=1 ./...` with `EACP_TEST_ADMIN_DSN` set (redirect to a workspace log; it takes several minutes).
Expected: every package `ok`, exit 0 (the worker isolation sweep needs no new fixture: 00021 adds no table).

- [ ] **Step 5: Commit**

```bash
git add test/demo/slice_c_test.go docs AGENTS.md
git commit -m "docs: ADR-026 Rev 1.1 Governance-as-Code for identity, policy, budgets and prices; Phase 21 delivered"
```
