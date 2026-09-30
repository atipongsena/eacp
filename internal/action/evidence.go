package action

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/audit"
	"github.com/atipongsena/eacp/internal/storage"
)

// DecisionView is one governance decision recorded for an action: the
// initial evaluation and every revalidation at the release boundary.
type DecisionView struct {
	ID                 uuid.UUID `json:"id"`
	DecisionID         uuid.UUID `json:"decision_id"`
	PolicyVersion      int       `json:"policy_version"`
	Provider           string    `json:"provider"`
	ProviderInstanceID string    `json:"provider_instance_id"`
	Verdict            string    `json:"verdict"`
	Reasons            []string  `json:"reasons"`
	InputDigest        string    `json:"input_digest"`
	EnforcedDigest     string    `json:"enforced_digest"`
	EvaluatedAt        time.Time `json:"evaluated_at"`
	RecordedAt         time.Time `json:"recorded_at"`
	// ProviderEvidence is what the provider used to decide (ADR-002 §8):
	// for AGT, the ACS identity, the matched rule and the engine versions.
	ProviderEvidence json.RawMessage `json:"provider_evidence,omitempty"`
}

// VoteView is one approver's vote.
type VoteView struct {
	ID       uuid.UUID `json:"id"`
	Approver uuid.UUID `json:"approver_principal_id"`
	Decision string    `json:"decision"`
	Reason   string    `json:"reason"`
	VotedAt  time.Time `json:"voted_at"`
}

// GrantView is the one-time grant an approval issued, and what consumed it.
type GrantView struct {
	ID                 uuid.UUID  `json:"id"`
	EnforcedDigest     string     `json:"enforced_digest"`
	PolicyVersion      int        `json:"policy_version"`
	CreatedAt          time.Time  `json:"created_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
	ConsumedByActionID *uuid.UUID `json:"consumed_by_action_id,omitempty"`
}

// ApprovalView is an approval request for an action with its votes and grant.
type ApprovalView struct {
	ID                  uuid.UUID  `json:"id"`
	State               string     `json:"state"`
	DecisionEvidenceID  uuid.UUID  `json:"decision_evidence_id"`
	PolicyVersion       int        `json:"policy_version"`
	EnforcedDigest      string     `json:"enforced_digest"`
	RequiredQuorum      int        `json:"required_quorum"`
	RequestingSubjectID uuid.UUID  `json:"requesting_subject_id"`
	CreatedAt           time.Time  `json:"created_at"`
	ExpiresAt           time.Time  `json:"expires_at"`
	Votes               []VoteView `json:"votes"`
	Grant               *GrantView `json:"grant,omitempty"`
}

// AttemptView is one dispatch of an action (its dispatch intent and result).
type AttemptView struct {
	AttemptNo         int        `json:"attempt"`
	LeaseGeneration   int64      `json:"lease_generation"`
	WorkerID          string     `json:"worker_id"`
	OperationKey      string     `json:"operation_key"`
	ContractVersion   int        `json:"connector_contract_version"`
	DispatchedAt      time.Time  `json:"dispatched_at"`
	CallDeadline      time.Time  `json:"call_deadline"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	Outcome           string     `json:"outcome,omitempty"`
	ExternalReference string     `json:"external_reference,omitempty"`
	ErrorClass        string     `json:"error_class,omitempty"`
	// RemoteReference is the remote agent's task id (ADR-030 §6): evidence
	// for settling an unknown outcome, never a proof of success.
	RemoteReference string `json:"remote_reference,omitempty"`
	Late            bool   `json:"late"`
}

// CheckView is one reconciliation lookup and what it saw.
type CheckView struct {
	LeaseGeneration   int64     `json:"lease_generation"`
	ReconcilerID      string    `json:"reconciler_id"`
	CheckedAt         time.Time `json:"checked_at"`
	Result            string    `json:"result"`
	ExternalReference string    `json:"external_reference,omitempty"`
	ProofStandard     string    `json:"proof_standard"`
	ContractVersion   int       `json:"connector_contract_version"`
}

// ReservationView is a budget reservation of the action (ADR-012): made
// at release, then committed on success or released without effect.
type ReservationView struct {
	ID              uuid.UUID   `json:"id"`
	AccountID       uuid.UUID   `json:"account_id"`
	ContractID      uuid.UUID   `json:"contract_id"`
	Unit            string      `json:"unit"`
	Amount          json.Number `json:"amount"`
	State           string      `json:"state"`
	CommittedAmount json.Number `json:"committed_amount,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	ExpiresAt       time.Time   `json:"expires_at"`
	SettledAt       *time.Time  `json:"settled_at,omitempty"`
	SettleReason    string      `json:"settle_reason,omitempty"`
}

// JournalActor is who made a journaled change.
type JournalActor struct {
	Kind   string    `json:"kind"`
	ID     uuid.UUID `json:"id"`
	Worker string    `json:"worker,omitempty"`
}

// JournalSubject is what a journaled change was made to.
type JournalSubject struct {
	Type string    `json:"type"`
	ID   uuid.UUID `json:"id"`
}

// JournalEntry is one link of the tenant's hash-chained journal (ADR-003)
// about the action or a record the action refers to.
type JournalEntry struct {
	Seq        int64           `json:"seq"`
	RecordedAt time.Time       `json:"recorded_at"`
	PrevHash   string          `json:"prev_hash"`
	Hash       string          `json:"hash"`
	Kind       string          `json:"event"`
	Actor      JournalActor    `json:"actor"`
	Subject    JournalSubject  `json:"subject"`
	Reason     string          `json:"reason"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// ChainView is the verification of the tenant's whole journal, made in the
// same snapshot as the evidence: the entries above are links of this chain.
type ChainView struct {
	Verified bool   `json:"verified"`
	Count    int64  `json:"count,omitempty"`
	Head     string `json:"head,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Evidence is everything recorded about one action (MASTER_PLAN §103
// invariants 10 and 17): governance decisions, approvals with their votes
// and grant, every attempt (including late results), every reconciliation
// check, every operator resolution, its budget reservations, and the
// journal entries about all of them, verified as one hash chain.
type Evidence struct {
	ActionID    uuid.UUID         `json:"action_id"`
	Action      View              `json:"action"`
	Decisions   []DecisionView    `json:"decisions"`
	Approvals   []ApprovalView    `json:"approvals"`
	Attempts    []AttemptView     `json:"attempts"`
	Checks      []CheckView       `json:"reconciliation_checks"`
	Resolutions []ResolutionView  `json:"resolutions"`
	Budget      []ReservationView `json:"budget_reservations"`
	Journal     []JournalEntry    `json:"journal"`
	Chain       ChainView         `json:"chain"`
	// Result is the kept output's metadata (ADR-034), never its content.
	Result *ResultMeta `json:"result,omitempty"`
}

// Evidence reconstructs action id from the database in one read-only
// snapshot. The journal chain is verified in that snapshot; a broken chain
// is reported in Chain, not as an error, so that the evidence stays
// readable when it matters most.
func (e *Engine) Evidence(ctx context.Context, tenant, id uuid.UUID) (Evidence, error) {
	ev := Evidence{ActionID: id, Decisions: []DecisionView{}, Approvals: []ApprovalView{}, Attempts: []AttemptView{},
		Checks: []CheckView{}, Resolutions: []ResolutionView{}, Budget: []ReservationView{}, Journal: []JournalEntry{}}
	err := storage.InTenantReadTx(ctx, e.pool, tenant.String(), func(tx pgx.Tx) error {
		r, err := load(ctx, tx, id, false)
		if err != nil {
			return err
		}
		ev.Action = r.view()
		subjects := []uuid.UUID{id}
		if ev.Decisions, err = decisions(ctx, tx, id); err != nil {
			return err
		}
		for _, d := range ev.Decisions {
			subjects = append(subjects, d.ID)
		}
		if ev.Approvals, err = approvals(ctx, tx, id); err != nil {
			return err
		}
		for _, a := range ev.Approvals {
			subjects = append(subjects, a.ID)
			for _, v := range a.Votes {
				subjects = append(subjects, v.ID)
			}
			if a.Grant != nil {
				subjects = append(subjects, a.Grant.ID)
			}
		}
		if ev.Attempts, err = attempts(ctx, tx, id); err != nil {
			return err
		}
		if ev.Checks, err = checks(ctx, tx, id); err != nil {
			return err
		}
		if ev.Result, err = resultMeta(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+resolutionColumns+` FROM eacp.action_resolutions
			WHERE action_id = $1 ORDER BY proposed_at, id`, id)
		if err != nil {
			return err
		}
		if ev.Resolutions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ResolutionView, error) {
			return scanResolution(r)
		}); err != nil {
			return err
		}
		for _, res := range ev.Resolutions {
			subjects = append(subjects, res.ID)
		}
		if ev.Budget, err = reservations(ctx, tx, id); err != nil {
			return err
		}
		for _, b := range ev.Budget {
			subjects = append(subjects, b.ID)
		}
		if ev.Journal, err = journal(ctx, tx, subjects); err != nil {
			return err
		}
		res, err := audit.Verify(ctx, tx)
		switch {
		case errors.Is(err, audit.ErrChainBroken):
			ev.Chain = ChainView{Error: err.Error()}
		case err != nil:
			return err
		default:
			ev.Chain = ChainView{Verified: true, Count: res.Count, Head: hex.EncodeToString(res.Head)}
		}
		return nil
	})
	if err != nil {
		return Evidence{}, mapErr(err)
	}
	return ev, nil
}

func decisions(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]DecisionView, error) {
	rows, err := tx.Query(ctx, `SELECT id, decision_id, policy_version, provider, provider_instance_id, verdict,
		reasons, input_digest, enforced_digest, evaluated_at, recorded_at, provider_evidence::text
		FROM eacp.decision_evidence WHERE action_id = $1 ORDER BY recorded_at, id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DecisionView, error) {
		var d DecisionView
		var in, enforced []byte
		var providerEvidence *string
		err := r.Scan(&d.ID, &d.DecisionID, &d.PolicyVersion, &d.Provider, &d.ProviderInstanceID, &d.Verdict,
			&d.Reasons, &in, &enforced, &d.EvaluatedAt, &d.RecordedAt, &providerEvidence)
		d.InputDigest, d.EnforcedDigest = hex.EncodeToString(in), hex.EncodeToString(enforced)
		if providerEvidence != nil {
			d.ProviderEvidence = json.RawMessage(*providerEvidence)
		}
		return d, err
	})
}

func approvals(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]ApprovalView, error) {
	rows, err := tx.Query(ctx, `SELECT id, state, decision_evidence_id, policy_version, enforced_digest,
		required_quorum, requesting_subject_id, created_at, expires_at
		FROM eacp.approval_requests WHERE action_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	reqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ApprovalView, error) {
		a := ApprovalView{Votes: []VoteView{}}
		var digest []byte
		err := r.Scan(&a.ID, &a.State, &a.DecisionEvidenceID, &a.PolicyVersion, &digest, &a.RequiredQuorum,
			&a.RequestingSubjectID, &a.CreatedAt, &a.ExpiresAt)
		a.EnforcedDigest = hex.EncodeToString(digest)
		return a, err
	})
	if err != nil {
		return nil, err
	}
	for i := range reqs {
		a := &reqs[i]
		rows, err := tx.Query(ctx, `SELECT id, approver_principal_id, decision, reason, voted_at
			FROM eacp.approval_votes WHERE request_id = $1 ORDER BY voted_at, id`, a.ID)
		if err != nil {
			return nil, err
		}
		if a.Votes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (VoteView, error) {
			var v VoteView
			err := r.Scan(&v.ID, &v.Approver, &v.Decision, &v.Reason, &v.VotedAt)
			return v, err
		}); err != nil {
			return nil, err
		}
		var g GrantView
		var digest []byte
		err = tx.QueryRow(ctx, `SELECT id, enforced_digest, policy_version, created_at, expires_at, consumed_at,
			consumed_by_action_id FROM eacp.approval_grants WHERE request_id = $1`, a.ID).
			Scan(&g.ID, &digest, &g.PolicyVersion, &g.CreatedAt, &g.ExpiresAt, &g.ConsumedAt, &g.ConsumedByActionID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			g.EnforcedDigest = hex.EncodeToString(digest)
			a.Grant = &g
		}
	}
	return reqs, nil
}

func attempts(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]AttemptView, error) {
	rows, err := tx.Query(ctx, `SELECT attempt_no, lease_generation, worker_id, operation_key,
		connector_contract_version, dispatched_at, call_deadline, completed_at, COALESCE(outcome, ''),
		COALESCE(external_reference, ''), COALESCE(error_class, ''), COALESCE(remote_reference, ''), late
		FROM eacp.action_attempts WHERE action_id = $1 ORDER BY attempt_no`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AttemptView, error) {
		var x AttemptView
		err := r.Scan(&x.AttemptNo, &x.LeaseGeneration, &x.WorkerID, &x.OperationKey, &x.ContractVersion,
			&x.DispatchedAt, &x.CallDeadline, &x.CompletedAt, &x.Outcome, &x.ExternalReference, &x.ErrorClass,
			&x.RemoteReference, &x.Late)
		return x, err
	})
}

func checks(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]CheckView, error) {
	rows, err := tx.Query(ctx, `SELECT lease_generation, reconciler_id, checked_at, result,
		COALESCE(external_reference, ''), proof_standard, connector_contract_version
		FROM eacp.reconciliation_checks WHERE action_id = $1 ORDER BY lease_generation`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CheckView, error) {
		var x CheckView
		err := r.Scan(&x.LeaseGeneration, &x.ReconcilerID, &x.CheckedAt, &x.Result, &x.ExternalReference,
			&x.ProofStandard, &x.ContractVersion)
		return x, err
	})
}

func reservations(ctx context.Context, tx pgx.Tx, id uuid.UUID) ([]ReservationView, error) {
	rows, err := tx.Query(ctx, `SELECT id, account_id, contract_id, unit, trim_scale(amount)::text, state,
		COALESCE(trim_scale(committed_amount)::text, ''), created_at, expires_at, settled_at, COALESCE(settle_reason, '')
		FROM eacp.budget_reservations WHERE action_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReservationView, error) {
		var x ReservationView
		var amount, committed string
		err := r.Scan(&x.ID, &x.AccountID, &x.ContractID, &x.Unit, &amount, &x.State, &committed, &x.CreatedAt,
			&x.ExpiresAt, &x.SettledAt, &x.SettleReason)
		x.Amount, x.CommittedAmount = json.Number(amount), json.Number(committed)
		return x, err
	})
}

// journal returns the tenant's journal entries whose subject is one of
// subjects, in chain order.
func journal(ctx context.Context, tx pgx.Tx, subjects []uuid.UUID) ([]JournalEntry, error) {
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		ids[i] = s.String()
	}
	rows, err := tx.Query(ctx, `SELECT seq, recorded_at, prev_hash, hash, payload FROM eacp.audit_events
		WHERE convert_from(payload, 'UTF8')::jsonb #>> '{subject,id}' = ANY($1::text[]) ORDER BY seq`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (JournalEntry, error) {
		var j JournalEntry
		var prev, hash, body []byte
		if err := r.Scan(&j.Seq, &j.RecordedAt, &prev, &hash, &body); err != nil {
			return j, err
		}
		j.PrevHash, j.Hash = hex.EncodeToString(prev), hex.EncodeToString(hash)
		var p struct {
			Action  string          `json:"action"`
			Actor   JournalActor    `json:"actor"`
			Subject JournalSubject  `json:"subject"`
			Reason  string          `json:"reason"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return j, err
		}
		j.Kind, j.Actor, j.Subject, j.Reason, j.Data = p.Action, p.Actor, p.Subject, p.Reason, p.Data
		return j, nil
	})
}
