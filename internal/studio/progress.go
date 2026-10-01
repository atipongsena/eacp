package studio

import (
	"context"
	"encoding/json"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Preview starts an owned approved version with private tool samples.
func (s *Service) Preview(ctx context.Context, a registry.Actor, version uuid.UUID, inputs, samples json.RawMessage) (Run, error) {
	if len(inputs) == 0 {
		inputs = json.RawMessage(`{}`)
	}
	if len(samples) == 0 {
		samples = json.RawMessage(`{}`)
	}
	var id uuid.UUID
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT eacp.studio_preview_start($1,$2::jsonb,$3::jsonb)`, version, string(inputs), string(samples)).Scan(&id)
	})
	if err != nil {
		return Run{}, err
	}
	return s.Run(ctx, a, id, false)
}

// NodeRequest names a node under the runtime's current lease.
type NodeRequest struct {
	Lease
	Index    int             `json:"index"`
	Result   json.RawMessage `json:"result,omitempty"`
	ActionID uuid.UUID       `json:"action_id,omitempty"`
}

// NodeAction records an action before waiting, without advancing the cursor.
func (s *Service) NodeAction(ctx context.Context, a registry.Actor, run uuid.UUID, n NodeRequest) error {
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_node_action($1,$2,$3,$4,$5)`, run, n.RuntimeID, n.Generation, n.Index, n.ActionID)
		return err
	})
}

// BeginNode returns only durable progress metadata. LLM intents use the
// same node id and are fenced again when the gateway admits a request.
func (s *Service) BeginNode(ctx context.Context, a registry.Actor, run uuid.UUID, n NodeRequest, llm bool) (json.RawMessage, error) {
	query := `SELECT eacp.studio_node_begin($1,$2,$3,$4)`
	if llm {
		query = `SELECT eacp.studio_llm_begin($1,$2,$3,$4)`
	}
	return s.nodeJSON(ctx, a, run, n, query)
}

// NodeOutput reads private content only through the live lease guard.
func (s *Service) NodeOutput(ctx context.Context, a registry.Actor, run uuid.UUID, n NodeRequest) (json.RawMessage, error) {
	return s.nodeJSON(ctx, a, run, n, `SELECT eacp.studio_node_output($1,$2,$3,$4)`)
}

func (s *Service) nodeJSON(ctx context.Context, a registry.Actor, run uuid.UUID, n NodeRequest, query string) (json.RawMessage, error) {
	var raw []byte
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, run, n.RuntimeID, n.Generation, n.Index).Scan(&raw)
	})
	return json.RawMessage(raw), err
}

// CompleteNode supplies only action identity or the fixed branch choice.
func (s *Service) CompleteNode(ctx context.Context, a registry.Actor, run uuid.UUID, n NodeRequest) error {
	if len(n.Result) == 0 {
		n.Result = json.RawMessage(`{}`)
	}
	return s.change(ctx, a, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT eacp.studio_node_complete($1,$2,$3,$4,$5::jsonb)`, run, n.RuntimeID, n.Generation, n.Index, string(n.Result))
		return err
	})
}
