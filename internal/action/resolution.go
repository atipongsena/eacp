package action

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// Resolution is an operator's resolution of a NEEDS_HUMAN_RESOLUTION action
// (ADR-004 T35-T37, MASTER_PLAN §20.3). Outcome is succeeded (with evidence
// and the external reference), failed (with evidence that it did not
// execute) or retry (proposed here, applied by a second operator).
type Resolution struct {
	Outcome           string
	Reason            string
	Evidence          string
	ExternalReference string
}

// ResolutionView is a stored resolution.
type ResolutionView struct {
	ID                uuid.UUID  `json:"id"`
	ActionID          uuid.UUID  `json:"action_id"`
	Outcome           string     `json:"outcome"`
	State             string     `json:"state"`
	Reason            string     `json:"reason"`
	Evidence          string     `json:"evidence,omitempty"`
	ExternalReference string     `json:"external_reference,omitempty"`
	ProposedBy        uuid.UUID  `json:"proposed_by"`
	ProposedAt        time.Time  `json:"proposed_at"`
	DecidedBy         *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt         *time.Time `json:"decided_at,omitempty"`
	DecisionReason    string     `json:"decision_reason,omitempty"`
}

const resolutionColumns = `id, action_id, outcome, state, reason, COALESCE(evidence, ''),
	COALESCE(external_reference, ''), proposed_by, proposed_at, decided_by, decided_at, COALESCE(decision_reason, '')`

func scanResolution(r pgx.Row) (ResolutionView, error) {
	var v ResolutionView
	err := r.Scan(&v.ID, &v.ActionID, &v.Outcome, &v.State, &v.Reason, &v.Evidence, &v.ExternalReference,
		&v.ProposedBy, &v.ProposedAt, &v.DecidedBy, &v.DecidedAt, &v.DecisionReason)
	return v, err
}

// Resolve records an operator's resolution. SUCCEEDED and FAILED apply at
// once (T35, T36); a retry is only proposed and needs DecideResolution by a
// second operator (T37). The database checks the operator role, separation
// of duties (not the subject, the agent's owner or its owner group), the
// evidence each outcome needs, and that the action needs a human.
func (e *Engine) Resolve(ctx context.Context, a Actor, id uuid.UUID, r Resolution) (View, ResolutionView, error) {
	if a.PrincipalID == uuid.Nil {
		return View{}, ResolutionView{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only operators resolve actions"}
	}
	switch r.Outcome {
	case "succeeded", "failed", "retry":
	default:
		return View{}, ResolutionView{}, invalid("outcome must be succeeded, failed or retry")
	}
	var v View
	var res ResolutionView
	err := e.inTx(ctx, a, func(tx pgx.Tx) error {
		if _, err := load(ctx, tx, id, true); err != nil { // the action first: lock order
			return err
		}
		var err error
		res, err = scanResolution(tx.QueryRow(ctx, `INSERT INTO eacp.action_resolutions
			(tenant_id, action_id, outcome, reason, evidence, external_reference)
			VALUES (eacp.current_tenant_id(), $1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
			RETURNING `+resolutionColumns, id, r.Outcome, r.Reason, r.Evidence, r.ExternalReference))
		if err != nil {
			return err
		}
		after, err := load(ctx, tx, id, false)
		v = after.view()
		return err
	})
	if err != nil {
		return View{}, ResolutionView{}, mapErr(err)
	}
	return v, res, nil
}

// DecideResolution applies (confirm) or withdraws a proposed retry. Only a
// second, distinct operator may apply it; applying moves the action to
// RETRY_WAIT with its operation key unchanged (T37).
func (e *Engine) DecideResolution(ctx context.Context, a Actor, actionID, resolutionID uuid.UUID, confirm bool,
	reason string) (View, ResolutionView, error) {
	if a.PrincipalID == uuid.Nil {
		return View{}, ResolutionView{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only operators resolve actions"}
	}
	state := "WITHDRAWN"
	if confirm {
		state = "APPLIED"
	}
	var v View
	var res ResolutionView
	err := e.inTx(ctx, a, func(tx pgx.Tx) error {
		if _, err := load(ctx, tx, actionID, true); err != nil {
			return err
		}
		var err error
		res, err = scanResolution(tx.QueryRow(ctx, `UPDATE eacp.action_resolutions
			SET state = $3, decision_reason = $4 WHERE id = $2 AND action_id = $1
			RETURNING `+resolutionColumns, actionID, resolutionID, state, reason))
		if errors.Is(err, pgx.ErrNoRows) {
			return &registry.Error{Kind: registry.ErrNotFound, Msg: "no such resolution"}
		}
		if err != nil {
			return err
		}
		after, err := load(ctx, tx, actionID, false)
		v = after.view()
		return err
	})
	if err != nil {
		return View{}, ResolutionView{}, mapErr(err)
	}
	return v, res, nil
}

var listableStates = map[string]bool{
	"RECEIVED": true, "PENDING_APPROVAL": true, "AUTHORIZED": true, "QUEUED": true, "LEASED": true,
	"EXECUTING": true, "RETRY_WAIT": true, "UNKNOWN_OUTCOME": true, "RECONCILING": true,
	"NEEDS_HUMAN_RESOLUTION": true, "SUCCEEDED": true, "FAILED": true, "DENIED": true, "CANCELLED": true,
	"EXPIRED": true,
}

// List returns up to limit (at most 200) of tenant's actions in state,
// oldest state change first: the operator's queue, e.g.
// NEEDS_HUMAN_RESOLUTION.
func (e *Engine) List(ctx context.Context, tenant uuid.UUID, state string, limit int) ([]View, error) {
	if !listableStates[state] {
		return nil, invalid("unknown action state %q", state)
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var views []View
	err := storage.InTenantTx(ctx, e.pool, tenant.String(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+rowColumns+` FROM eacp.actions
			WHERE state = $1 ORDER BY state_changed_at, id LIMIT $2`, state, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRow(rows)
			if err != nil {
				return err
			}
			views = append(views, r.view())
		}
		return rows.Err()
	})
	return views, mapErr(err)
}
