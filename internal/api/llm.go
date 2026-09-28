package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/llm"
	"github.com/atipongsena/eacp/internal/registry"
)

// llmCallReader may read the LLM-call ledger (ADR-031).
var llmCallReader = []string{"admin", "operator", "auditor"}

func (s *Server) registerLLM(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("POST /v1/llm-models", p(editor, s.registerLLMModel))
	mux.Handle("GET /v1/llm-models", p(anyPrincipal, s.listLLMModels))
	mux.Handle("GET /v1/llm-calls", p(llmCallReader, s.listLLMCalls))
	mux.Handle("GET /v1/llm-calls/{id}", p(llmCallReader, s.getLLMCall))
}

func (s *Server) registerLLMModel(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in registry.NewLLMModel
	if err := decode(r, &in); err != nil {
		return err
	}
	m, err := s.reg.RegisterLLMModel(r.Context(), actor(c), in)
	return created(w, m, err)
}

func (s *Server) listLLMModels(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	list, err := s.reg.ListLLMModels(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"models": list})
	}
	return err
}

func (s *Server) listLLMCalls(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	q := r.URL.Query()
	f := llm.Filter{Model: q.Get("model"), State: q.Get("state")}
	if raw := q.Get("agent"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return badRequest{"agent must be a uuid"}
		}
		f.AgentID = id
	}
	for key, dst := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if raw := q.Get(key); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return badRequest{key + " must be an RFC 3339 time"}
			}
			*dst = t
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return badRequest{"limit must be between 1 and 1000"}
		}
		f.Limit = n
	}
	calls, err := s.llm.List(r.Context(), c.TenantID, f)
	if err == nil {
		if calls == nil {
			calls = []llm.Call{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"calls": calls})
	}
	return err
}

func (s *Server) getLLMCall(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	call, err := s.llm.Get(r.Context(), c.TenantID, id)
	if err == nil {
		writeJSON(w, http.StatusOK, call)
	}
	return err
}
