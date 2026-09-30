package action

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/atipongsena/eacp/internal/registry"
	"github.com/atipongsena/eacp/internal/storage"
)

// ErrResultNotAvailable: the action has no result the caller may read (none
// was kept, it expired, or the action is another agent's) (ADR-034).
var ErrResultNotAvailable = errors.New("result_not_available")

// WithheldError: the call succeeded but its output was not kept (ADR-034).
type WithheldError struct{ Reason string }

func (e *WithheldError) Error() string { return "result withheld: " + e.Reason }

// ResultView is a success's output as its calling agent reads it (ADR-034).
type ResultView struct {
	ActionID  uuid.UUID       `json:"action_id"`
	Output    json.RawMessage `json:"output"`
	SHA256    string          `json:"sha256"`
	Bytes     int             `json:"bytes"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// ResultMeta is what evidence shows of a result: never its content.
type ResultMeta struct {
	Withheld  *string    `json:"withheld,omitempty"`
	SHA256    *string    `json:"sha256,omitempty"`
	Bytes     *int       `json:"bytes,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	PrunedAt  *time.Time `json:"pruned_at,omitempty"`
}

// Result returns action id's kept output to the agent that submitted it.
// PostgreSQL decides who reads it (eacp.action_result): only an ACTIVE
// version of the action's agent, and only before it expires.
func (e *Engine) Result(ctx context.Context, a Actor, id uuid.UUID) (ResultView, error) {
	if !a.isAgent() {
		return ResultView{}, &registry.Error{Kind: registry.ErrForbidden, Msg: "only the calling agent reads a result"}
	}
	v := ResultView{ActionID: id}
	var output, withheld, digest *string
	var bytes *int
	err := storage.InTenantTx(ctx, e.pool, a.TenantID.String(), func(tx pgx.Tx) error {
		if err := storage.SetAgent(ctx, tx, a.AgentVersionID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT output, sha256, bytes, expires_at, withheld FROM eacp.action_result($1)`, id).
			Scan(&output, &digest, &bytes, &v.ExpiresAt, &withheld)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ResultView{}, ErrResultNotAvailable
	case err != nil:
		return ResultView{}, mapErr(err)
	case withheld != nil:
		return ResultView{}, &WithheldError{Reason: *withheld}
	case output == nil || digest == nil || bytes == nil:
		return ResultView{}, ErrResultNotAvailable
	}
	v.Output, v.SHA256, v.Bytes = json.RawMessage(*output), *digest, *bytes
	return v, nil
}

// resultMeta reads the metadata of action id's result, if any.
func resultMeta(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*ResultMeta, error) {
	var m ResultMeta
	err := tx.QueryRow(ctx, `SELECT withheld, sha256, bytes, created_at, expires_at, pruned_at
		FROM eacp.action_results WHERE action_id = $1`, id).
		Scan(&m.Withheld, &m.SHA256, &m.Bytes, &m.CreatedAt, &m.ExpiresAt, &m.PrunedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
