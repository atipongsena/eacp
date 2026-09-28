package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/release"
)

// Release roles (ADR-018 §5): the registry opens and records evidence, a
// registry_approver advances, operators and approvers roll back, and
// auditors review.
var (
	releaseReader   = []string{"registry_editor", "registry_approver", "operator", "auditor"}
	releaseRollback = containment
)

// WithReleases replaces the release service (the default evaluates
// observations with the local PDP).
func (s *Server) WithReleases(r *release.Service) *Server {
	s.releases = r
	return s
}

func (s *Server) registerRelease(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("POST /v1/releases", p(editorOrApprover, s.openRelease))
	mux.Handle("GET /v1/releases", p(releaseReader, s.listReleases))
	mux.Handle("GET /v1/releases/{id}", p(releaseReader, s.getRelease))
	mux.Handle("POST /v1/releases/{id}/evaluations", p(editorOrApprover, s.recordEvaluation))
	mux.Handle("POST /v1/releases/{id}/advance", p(approver, s.advanceRelease))
	mux.Handle("POST /v1/releases/{id}/rollback", p(releaseRollback, s.rollbackRelease))
	mux.Handle("POST /v1/agent/release/observations", s.agent(s.observeRelease))
	mux.Handle("GET /v1/agent/release/route", s.agent(s.releaseRoute))
}

func releaseAgent(c identity.Caller) release.Agent {
	return release.Agent{TenantID: c.TenantID, AgentID: c.AgentID, VersionID: c.AgentVersionID}
}

func (s *Server) openRelease(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in release.OpenRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	rel, err := s.releases.Open(r.Context(), actor(c), in)
	return created(w, rel, err)
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	f := release.Filter{State: r.URL.Query().Get("state")}
	if v := r.URL.Query().Get("agent_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return badRequest{"agent_id must be a UUID"}
		}
		f.AgentID = id
	}
	list, err := s.releases.List(r.Context(), actor(c), f)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"releases": list})
	}
	return err
}

func (s *Server) getRelease(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := s.releases.Get(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, d)
	}
	return err
}

func (s *Server) recordEvaluation(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in release.EvaluationInput
	if err := decode(r, &in); err != nil {
		return err
	}
	e, err := s.releases.RecordEvaluation(r.Context(), actor(c), id, in)
	return created(w, e, err)
}

// advanceRelease takes {"from", "from_canary_bp", "reason"}: the state the
// approver reviewed. A release that has moved since is a 409.
func (s *Server) advanceRelease(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in release.Advance
	if err := decode(r, &in); err != nil {
		return err
	}
	rel, err := s.releases.Advance(r.Context(), actor(c), id, in)
	if err == nil {
		writeJSON(w, http.StatusOK, rel)
	}
	return err
}

func (s *Server) rollbackRelease(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	rel, err := s.releases.Rollback(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, rel)
	}
	return err
}

// observeRelease records a candidate's replay or shadow proposal. When the
// PDP is unavailable nothing is recorded (503); the candidate may retry.
func (s *Server) observeRelease(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in release.Proposal
	if err := decode(r, &in); err != nil {
		return err
	}
	o, err := s.releases.Observe(r.Context(), releaseAgent(c), in)
	if errors.Is(err, release.ErrGovernanceUnavailable) {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "governance_unavailable"})
		return nil
	}
	return created(w, o, err)
}

func (s *Server) releaseRoute(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	rt, err := s.releases.Route(r.Context(), releaseAgent(c), r.URL.Query().Get("subject"))
	if err == nil {
		writeJSON(w, http.StatusOK, rt)
	}
	return err
}
