package api

import (
	"encoding/json"
	"net/http"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/studio"
)

// Agent Studio (Phase 27a-1, ADR-033). Authors save; approvers decide;
// PostgreSQL enforces every rule again.
var (
	studioAuthor = []string{"studio_author"}
	studioReader = []string{"studio_author", "registry_approver", "auditor"}
)

func (s *Server) registerStudio(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("POST /v1/studio/agents", p(studioAuthor, s.studioSave))
	mux.Handle("GET /v1/studio/agents", p(studioReader, s.studioAgents))
	mux.Handle("POST /v1/studio/agents/{id}/versions", p(studioAuthor, s.studioAddVersion))
	mux.Handle("GET /v1/studio/versions/{id}", p(studioReader, s.studioVersion))
	mux.Handle("GET /v1/studio/requests", p(approver, s.studioRequests))
	mux.Handle("POST /v1/studio/versions/{id}/approve", p(approver, s.studioDecide(true)))
	mux.Handle("POST /v1/studio/versions/{id}/reject", p(approver, s.studioDecide(false)))
}

// seesAll reports whether c reads every Studio agent, not only its own.
func seesAll(c identity.Caller) bool { return c.HasRole("registry_approver", "auditor") }

func (s *Server) studioSave(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in studio.NewAgent
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.studio.Save(r.Context(), actor(c), in)
	return created(w, v, err)
}

func (s *Server) studioAddVersion(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agent, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Definition json.RawMessage `json:"definition"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.studio.AddVersion(r.Context(), actor(c), agent, in.Definition)
	return created(w, v, err)
}

func (s *Server) studioAgents(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agents, err := s.studio.Agents(r.Context(), actor(c), seesAll(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
	}
	return err
}

func (s *Server) studioVersion(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	v, err := s.studio.Version(r.Context(), actor(c), id, seesAll(c))
	if err == nil {
		writeJSON(w, http.StatusOK, v)
	}
	return err
}

func (s *Server) studioRequests(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	requests, err := s.studio.Requests(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
	}
	return err
}

func (s *Server) studioDecide(approve bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		v, err := s.studio.Decide(r.Context(), actor(c), id, approve, in.Reason)
		if err == nil {
			writeJSON(w, http.StatusOK, v)
		}
		return err
	}
}
