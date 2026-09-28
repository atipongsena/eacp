package api

import (
	"encoding/json"
	"net/http"

	"github.com/atipongsena/eacp/internal/approval"
	"github.com/atipongsena/eacp/internal/identity"
)

func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Content json.RawMessage `json:"content"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.gov.CreatePolicy(r.Context(), actor(c), in.Content)
	return created(w, p, err)
}

func (s *Server) activatePolicy(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.gov.ActivatePolicy(r.Context(), actor(c), id, in.Reason))
}

func (s *Server) currentPolicy(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	p, err := s.gov.CurrentPolicy(r.Context(), c.TenantID)
	if err == nil {
		writeJSON(w, http.StatusOK, p)
	}
	return err
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	view, err := s.appr.GetEligible(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, view)
	}
	return err
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	items, err := s.appr.ListEligible(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
	return err
}

func (s *Server) voteApproval(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Decision approval.VoteDecision `json:"decision"`
		Reason   string                `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	result, err := s.appr.Vote(r.Context(), actor(c), id, in.Decision, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, result)
	}
	return err
}
