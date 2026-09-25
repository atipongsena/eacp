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
