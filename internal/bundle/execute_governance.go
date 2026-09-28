package bundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/budget"
	"github.com/atipongsena/eacp/internal/finops"
	"github.com/atipongsena/eacp/internal/governance"
	"github.com/atipongsena/eacp/internal/registry"
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
