package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/registry"
)

func (s *Server) registerDependency(mux *http.ServeMux) {
	mux.Handle("POST /v1/dependencies", s.principal(editor, s.recordDependency))
	mux.Handle("POST /v1/dependencies/{id}/revoke", s.principal(editor, s.revokeDependency))
	mux.Handle("GET /v1/dependencies/blast-radius", s.principal(actionReader, s.dependencyBlastRadius))
}

func (s *Server) recordDependency(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in registry.DependencyInput
	if err := decode(r, &in); err != nil {
		return err
	}
	edge, err := s.reg.RecordDependency(r.Context(), actor(c), in)
	if err == nil {
		writeJSON(w, http.StatusCreated, edge)
	}
	return err
}

func (s *Server) revokeDependency(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return badRequest{"reason is required"}
	}
	err = s.reg.RevokeDependency(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
	}
	return err
}

func (s *Server) dependencyBlastRadius(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	q := r.URL.Query()
	target := registry.DependencyTarget{Kind: q.Get("kind"), Name: q.Get("name")}
	if id := q.Get("id"); id != "" {
		var err error
		target.ID, err = uuid.Parse(id)
		if err != nil {
			return badRequest{"id must be a UUID"}
		}
	}
	report, err := s.reg.BlastRadius(r.Context(), actor(c), target)
	if err == nil {
		writeJSON(w, http.StatusOK, report)
	}
	return err
}
