package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/studio"
)

// The Agent Hub (Phase 27b, ADR-033 Rev 1.3). Any principal reaches these
// routes; PostgreSQL decides who proposes, approves, sees, runs and clones.
func (s *Server) registerStudioHub(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("GET /v1/studio/hub", p(anyPrincipal, s.studioHub))
	mux.Handle("GET /v1/studio/hub/{id}", p(anyPrincipal, s.studioHubListing))
	mux.Handle("GET /v1/studio/agents/{id}/listing", p(studioReader, s.studioAgentListing))
	mux.Handle("POST /v1/studio/agents/{id}/listing", p(studioAuthor, s.studioProposeListing))
	mux.Handle("GET /v1/studio/listing-requests", p(anyPrincipal, s.studioListingRequests))
	mux.Handle("POST /v1/studio/listing-proposals/{id}/approve", p(anyPrincipal, s.studioDecideListing(true)))
	mux.Handle("POST /v1/studio/listing-proposals/{id}/reject", p(anyPrincipal, s.studioDecideListing(false)))
	mux.Handle("POST /v1/studio/listing-proposals/{id}/cancel", p(studioAuthor, s.studioCancelListing))
	mux.Handle("POST /v1/studio/listings/{id}/deprecate", p(anyPrincipal, s.studioRetireListing("DEPRECATED")))
	mux.Handle("POST /v1/studio/listings/{id}/withdraw", p(anyPrincipal, s.studioRetireListing("WITHDRAWN")))
	mux.Handle("POST /v1/studio/listings/{id}/clone", p(studioAuthor, s.studioClone))
}

func (s *Server) studioHub(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	q := studio.HubQuery{Text: r.URL.Query().Get("q"), Tag: r.URL.Query().Get("tag")}
	if raw := r.URL.Query().Get("department"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return badRequest{"department must be a UUID"}
		}
		q.Department = id
	}
	listings, err := s.studio.Hub(r.Context(), actor(c), q)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"listings": listings})
	}
	return err
}

func (s *Server) studioHubListing(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	l, err := s.studio.HubListing(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, l)
	}
	return err
}

func (s *Server) studioAgentListing(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	l, err := s.studio.AgentListing(r.Context(), actor(c), id, seesAll(c))
	if err == nil {
		writeJSON(w, http.StatusOK, l)
	}
	return err
}

func (s *Server) studioProposeListing(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agent, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in studio.ProposeIn
	if err := decode(r, &in); err != nil {
		return err
	}
	p, err := s.studio.ProposeListing(r.Context(), actor(c), agent, in)
	return created(w, p, err)
}

func (s *Server) studioListingRequests(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	proposals, err := s.studio.ListingRequests(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"proposals": proposals})
	}
	return err
}

func (s *Server) studioDecideListing(approve bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		state, err := s.studio.DecideListing(r.Context(), actor(c), id, approve, in.Reason)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]string{"state": state})
		}
		return err
	}
}

func (s *Server) studioCancelListing(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.studio.CancelListing(r.Context(), actor(c), id, in.Reason))
}

func (s *Server) studioRetireListing(state string) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		got, err := s.studio.RetireListing(r.Context(), actor(c), id, state, in.Reason)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]string{"state": got})
		}
		return err
	}
}

func (s *Server) studioClone(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in studio.CloneIn
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.studio.Clone(r.Context(), actor(c), id, in)
	return created(w, v, err)
}
