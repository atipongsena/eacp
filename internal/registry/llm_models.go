package registry

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// NewLLMModel declares a model the LLM gateway may call (ADR-031). SecretRef
// names the provider key in the gateway's secrets file; the key itself never
// reaches the registry. TimeoutMS 0 takes the default (10 minutes).
type NewLLMModel struct {
	Name            string `json:"name"`
	Provider        string `json:"provider"` // anthropic or openai
	BaseURL         string `json:"base_url"`
	UpstreamModel   string `json:"upstream_model"`
	SecretRef       string `json:"secret_ref"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
}

// LLMModel is a declared, immutable model.
type LLMModel struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	Provider        string    `json:"provider"`
	BaseURL         string    `json:"base_url"`
	UpstreamModel   string    `json:"upstream_model"`
	SecretRef       string    `json:"secret_ref"`
	MaxOutputTokens int64     `json:"max_output_tokens"`
	TimeoutMS       int       `json:"timeout_ms"`
	CreatedBy       uuid.UUID `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
}

const llmModelColumns = `id, name, provider, base_url, upstream_model, secret_ref, max_output_tokens, timeout_ms,
	created_by, created_at`

func scanLLMModel(row pgx.Row) (LLMModel, error) {
	var m LLMModel
	err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.BaseURL, &m.UpstreamModel, &m.SecretRef, &m.MaxOutputTokens,
		&m.TimeoutMS, &m.CreatedBy, &m.CreatedAt)
	return m, err
}

// RegisterLLMModel inserts an immutable model (registry_editor).
func (t Tx) RegisterLLMModel(ctx context.Context, n NewLLMModel) (LLMModel, error) {
	var timeout any
	if n.TimeoutMS != 0 {
		timeout = n.TimeoutMS
	}
	return scanLLMModel(t.QueryRow(ctx, `
		INSERT INTO eacp.llm_models (tenant_id, name, provider, base_url, upstream_model, secret_ref,
		                             max_output_tokens, timeout_ms)
		VALUES (eacp.current_tenant_id(), $1, $2, $3, $4, $5, $6, COALESCE($7::integer, 600000))
		RETURNING `+llmModelColumns,
		n.Name, n.Provider, n.BaseURL, n.UpstreamModel, n.SecretRef, n.MaxOutputTokens, timeout))
}

// RegisterLLMModel declares a model the LLM gateway may call.
func (s *Service) RegisterLLMModel(ctx context.Context, a Actor, n NewLLMModel) (LLMModel, error) {
	var m LLMModel
	err := s.change(ctx, a, func(tx pgx.Tx) error {
		var err error
		m, err = Tx{tx}.RegisterLLMModel(ctx, n)
		return err
	})
	return m, err
}

// ListLLMModels lists the tenant's models by name.
func (s *Service) ListLLMModels(ctx context.Context, a Actor) ([]LLMModel, error) {
	out := []LLMModel{}
	err := s.read(ctx, a, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+llmModelColumns+` FROM eacp.llm_models ORDER BY name`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (LLMModel, error) { return scanLLMModel(r) })
		return err
	})
	return out, err
}

func resolveModel(ctx context.Context, tx pgx.Tx, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM eacp.llm_models WHERE name = $1`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, newErr(ErrNotFound, "unknown model %q", name)
	}
	return id, err
}
