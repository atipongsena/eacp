package studio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
)

// Run is a Studio run as a caller sees it. Answer is set only for the
// requester, until it expires (migration 00028).
type Run struct {
	ID              uuid.UUID  `json:"id"`
	AgentID         uuid.UUID  `json:"agent_id"`
	VersionID       uuid.UUID  `json:"version_id"`
	RequestedBy     uuid.UUID  `json:"requested_by"`
	State           string     `json:"state"`
	Mode            string     `json:"mode"`
	CurrentIndex    int        `json:"current_index"`
	FailureReason   string     `json:"failure_reason,omitempty"`
	Deadline        time.Time  `json:"deadline"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Steps           []RunStep  `json:"steps"`
	Nodes           []RunNode  `json:"nodes"`
	Answer          *string    `json:"answer,omitempty"`
	AnswerExpiresAt *time.Time `json:"answer_expires_at,omitempty"`
}

// RunStep is a recorded tool_call step and its action's current state.
type RunStep struct {
	Index       int       `json:"index"`
	StepID      string    `json:"step_id"`
	ActionID    uuid.UUID `json:"action_id"`
	ActionState string    `json:"action_state"`
}

// RunNode exposes graph progress metadata, never model output or samples.
type RunNode struct {
	Index         int        `json:"index"`
	StepID        string     `json:"step_id"`
	Kind          string     `json:"kind"`
	State         string     `json:"state"`
	ActionID      *uuid.UUID `json:"action_id,omitempty"`
	CallID        *uuid.UUID `json:"call_id,omitempty"`
	ActionState   string     `json:"action_state,omitempty"`
	CallState     string     `json:"call_state,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
}

// Start starts a run of agent with inputs as a (a department member).
func (s *Service) Start(ctx context.Context, a registry.Actor, agent uuid.UUID, inputs json.RawMessage) (Run, error) {
	if len(inputs) == 0 {
		inputs = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	if err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_run_start($1, $2::jsonb)`, agent, string(inputs)).Scan(&id)
	}); err != nil {
		return Run{}, err
	}
	return s.Run(ctx, a, id, false)
}

// Run returns run id to its requester, or to anyone when reader. Only the
// requester gets the answer (eacp.studio_run_answer decides).
func (s *Service) Run(ctx context.Context, a registry.Actor, id uuid.UUID, reader bool) (Run, error) {
	var r Run
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var reason *string
		var expires *time.Time
		if err := tx.QueryRow(ctx, `SELECT id, agent_id, version_id, requested_by, state, failure_reason, deadline,
				created_at, finished_at, answer_expires_at, mode, current_index
			FROM eacp.studio_runs WHERE id = $1 AND ($2 OR requested_by = $3)`, id, reader, a.PrincipalID).Scan(
			&r.ID, &r.AgentID, &r.VersionID, &r.RequestedBy, &r.State, &reason, &r.Deadline, &r.CreatedAt,
			&r.FinishedAt, &expires, &r.Mode, &r.CurrentIndex); err != nil {
			return err
		}
		if reason != nil {
			r.FailureReason = *reason
		}
		rows, err := tx.Query(ctx, `SELECT s.step_index, s.step_id, s.action_id, a.state
			FROM eacp.studio_run_steps s JOIN eacp.actions a ON a.tenant_id = s.tenant_id AND a.id = s.action_id
			WHERE s.run_id = $1 ORDER BY s.step_index`, id)
		if err != nil {
			return err
		}
		r.Steps, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunStep, error) {
			var st RunStep
			return st, row.Scan(&st.Index, &st.StepID, &st.ActionID, &st.ActionState)
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT n.step_index,n.step_id,n.kind,n.state,n.action_id,n.call_id,
			COALESCE(a.state,''),COALESCE(c.state,''),COALESCE(c.denial,n.failure_reason,'')
			FROM eacp.studio_run_nodes n
			LEFT JOIN eacp.actions a ON a.tenant_id=n.tenant_id AND a.id=n.action_id
			LEFT JOIN eacp.llm_calls c ON c.tenant_id=n.tenant_id AND c.id=n.call_id
			WHERE n.run_id=$1 ORDER BY n.step_index`, id)
		if err != nil {
			return err
		}
		r.Nodes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RunNode, error) {
			var n RunNode
			return n, row.Scan(&n.Index, &n.StepID, &n.Kind, &n.State, &n.ActionID, &n.CallID, &n.ActionState, &n.CallState, &n.FailureReason)
		})
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT eacp.studio_run_answer($1)`, id).Scan(&r.Answer); err != nil {
			return err
		}
		if r.Answer != nil {
			r.AnswerExpiresAt = expires
		}
		return nil
	})
	return r, err
}

// ClaimRequest is the runtime's claim.
type ClaimRequest struct {
	RuntimeID     string `json:"runtime_id"`
	MasterVersion string `json:"master_version"`
	LeaseSeconds  int    `json:"lease_seconds"`
	Limit         int    `json:"limit"`
}

// Claim leases runs for the runtime a. Each is the JSON object
// eacp.studio_run_claim builds.
func (s *Service) Claim(ctx context.Context, a registry.Actor, c ClaimRequest) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT eacp.studio_run_claim($1, $2, $3, $4)`,
			c.RuntimeID, c.MasterVersion, c.LeaseSeconds, c.Limit)
		if err != nil {
			return err
		}
		raw, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		for _, r := range raw {
			out = append(out, json.RawMessage(r))
		}
		return err
	})
	return out, err
}

// Lease names the runtime instance and generation that hold a run.
type Lease struct {
	RuntimeID  string `json:"runtime_id"`
	Generation int64  `json:"generation"`
}

// What a heartbeat reports (eacp.studio_run_heartbeat).
const (
	// BeatActive: the lease is extended and the run's version is ACTIVE.
	BeatActive = "active"
	// BeatReplaced: the run's version is no longer ACTIVE.
	BeatReplaced = "replaced"
	// BeatKilled: a kill matches the run and PostgreSQL failed it killed
	// (Phase 28); the runtime holds nothing more.
	BeatKilled = "killed"
)

// Heartbeat extends the runtime's lease on run and reports BeatActive,
// BeatReplaced or BeatKilled.
func (s *Service) Heartbeat(ctx context.Context, a registry.Actor, run uuid.UUID, l Lease, seconds int) (string, error) {
	var status string
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_run_heartbeat($1, $2, $3, $4)`,
			run, l.RuntimeID, l.Generation, seconds).Scan(&status)
	})
	return status, err
}

// Step records the action of run's step index.
func (s *Service) Step(ctx context.Context, a registry.Actor, run uuid.UUID, l Lease, index int, act uuid.UUID) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_run_step($1, $2, $3, $4, $5)`, run, l.RuntimeID, l.Generation, index, act)
		return err
	})
}

// Finish ends run as SUCCEEDED with answer, or FAILED with reason.
func (s *Service) Finish(ctx context.Context, a registry.Actor, run uuid.UUID, l Lease, state string, answer, reason *string) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_run_finish($1, $2, $3, $4, $5, $6)`,
			run, l.RuntimeID, l.Generation, state, answer, reason)
		return err
	})
}

// DueCredentials lists the versions needing a key under master.
func (s *Service) DueCredentials(ctx context.Context, a registry.Actor, master string) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT eacp.studio_credentials_due($1)`, master)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return out, err
}

// Proposal is a derived key the runtime proposes: only its hash.
type Proposal struct {
	VersionID     uuid.UUID `json:"version_id"`
	ID            uuid.UUID `json:"id"`
	Hash          string    `json:"hash"`
	MasterVersion string    `json:"master_version"`
}

// Propose proposes p as the runtime a.
func (s *Service) Propose(ctx context.Context, a registry.Actor, p Proposal) error {
	hash, err := hex.DecodeString(p.Hash)
	if err != nil || len(hash) != 32 {
		return &registry.Error{Kind: registry.ErrInvalid, Msg: "hash must be 64 hex characters (SHA-256)"}
	}
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_credential_propose($1, $2, $3, $4)`, p.VersionID, p.ID, hash, p.MasterVersion)
		return err
	})
}

// RevokeAll revokes every live Studio key as the operator a.
func (s *Service) RevokeAll(ctx context.Context, a registry.Actor, reason string) (int, error) {
	var n int
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_credentials_revoke_all($1)`, reason).Scan(&n)
	})
	return n, err
}

// KeyRequest is a pending key proposal for a Studio version, shown beside
// the capability requests (ADR-033 §3).
type KeyRequest struct {
	ID                uuid.UUID `json:"id"`
	VersionID         uuid.UUID `json:"version_id"`
	AgentID           uuid.UUID `json:"agent_id"`
	AgentName         string    `json:"agent_name"`
	Version           int       `json:"version"`
	Digest            string    `json:"digest"`
	Capability        []string  `json:"capability"`
	Models            []string  `json:"models"`
	MasterVersion     string    `json:"master_version,omitempty"`
	ProposedBy        uuid.UUID `json:"proposed_by"`
	ProposedByRuntime bool      `json:"proposed_by_runtime"`
	ProposedAt        time.Time `json:"proposed_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// KeyRequests lists pending keys of Studio versions, oldest first.
func (s *Service) KeyRequests(ctx context.Context, a registry.Actor) ([]KeyRequest, error) {
	out := []KeyRequest{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.id, c.agent_version_id, v.agent_id, g.name, v.version, s.digest, s.capability,
				ARRAY(SELECT m.name FROM eacp.llm_models m WHERE m.tenant_id=s.tenant_id AND m.id=ANY(s.model_capability) ORDER BY m.name),
				COALESCE(sc.master_version, ''), c.proposed_by, sc.id IS NOT NULL, c.proposed_at, c.expires_at
			FROM eacp.credentials c
			JOIN eacp.studio_versions s ON s.tenant_id = c.tenant_id AND s.id = c.agent_version_id
			JOIN eacp.agent_versions v ON v.tenant_id = c.tenant_id AND v.id = c.agent_version_id
			JOIN eacp.agents g ON g.tenant_id = v.tenant_id AND g.id = v.agent_id
			LEFT JOIN eacp.studio_credentials sc ON sc.tenant_id = c.tenant_id AND sc.id = c.id
			WHERE c.kind = 'ak' AND c.approved_at IS NULL AND c.revoked_at IS NULL AND c.expires_at > now()
			ORDER BY c.proposed_at, c.id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (KeyRequest, error) {
			var k KeyRequest
			return k, r.Scan(&k.ID, &k.VersionID, &k.AgentID, &k.AgentName, &k.Version, &k.Digest, &k.Capability, &k.Models,
				&k.MasterVersion, &k.ProposedBy, &k.ProposedByRuntime, &k.ProposedAt, &k.ExpiresAt)
		})
		return err
	})
	return out, err
}
