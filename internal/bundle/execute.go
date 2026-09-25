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
	for _, st := range steps {
		if st.Stage != stage {
			continue
		}
		obj, err := apply(ctx, tx, st, ids, produced)
		if err != nil {
			mapped := registry.MapErr(err)
			if st.Op == OpTransition && errors.Is(mapped, registry.ErrConflict) {
				return &Error{Code: CodeStale, Address: st.Address, Err: mapped,
					Msg: fmt.Sprintf("step %d (%s %s): %v: plan the bundle again", st.Ordinal, st.Op, st.Address, mapped)}
			}
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
		if kind, _, _ := strings.Cut(st.Address, "."); managedKinds[kind] {
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

// managedKinds are the kinds whose created or imported objects a bundle
// manages (bundle_resources).
var managedKinds = map[string]bool{"connector": true, "tool": true, "agent": true, "version": true,
	"principal": true, "group": true, "policy": true, "budget": true, "price": true}

// apply makes one step's registry write and returns the object it produced
// or acted on.
func apply(ctx context.Context, tx pgx.Tx, st Step, ids map[string]uuid.UUID,
	produced map[int]uuid.UUID) (uuid.UUID, error) {
	rtx := registry.Tx{Tx: tx}
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
		return pid, rtx.TransitionVersionFrom(ctx, pid, p.From, p.To, p.Reason)
	case "revoke contract":
		if p.ObjectID == nil {
			return uuid.Nil, errPayload
		}
		return *p.ObjectID, rtx.RevokeContract(ctx, *p.ObjectID, p.Reason)
	}
	if id, ok, err := applyGovernance(ctx, tx, st, ids, produced); ok {
		return id, err
	}
	if st.Op == OpImport && p.ObjectID != nil {
		return *p.ObjectID, nil
	}
	return uuid.Nil, fmt.Errorf("unknown step %s %s", st.Op, st.Address)
}
