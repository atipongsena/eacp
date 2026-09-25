package api

import (
	"net/http"
	"strconv"

	"eacp/internal/bundle"
	"eacp/internal/identity"
)

// Governance-as-Code (ADR-026). Planning and submitting are open to those
// who write the registry; approval to registry approvers and admins (the
// registry triggers still check each step's own role).
var (
	bundleWriter   = []string{"registry_editor", "registry_approver", "admin"}
	bundleApprover = []string{"registry_approver", "admin"}
	bundleReader   = []string{"registry_editor", "registry_approver", "admin", "auditor"}
)

func (s *Server) registerBundles(mux *http.ServeMux) {
	mux.Handle("POST /v1/change-sets", s.principal(bundleWriter, s.planChangeSet))
	mux.Handle("GET /v1/change-sets", s.principal(bundleReader, s.listChangeSets))
	mux.Handle("GET /v1/change-sets/{id}", s.principal(bundleReader, s.getChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/submit", s.principal(bundleWriter, s.submitChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/approve", s.principal(bundleApprover, s.approveChangeSet))
	mux.Handle("POST /v1/change-sets/{id}/reject", s.principal(bundleWriter, s.rejectChangeSet))
	mux.Handle("GET /v1/bundles", s.principal(bundleReader, s.listBundles))
	mux.Handle("GET /v1/bundles/{name}/drift", s.principal(bundleReader, s.bundleDrift))
}

// bundleStatus is the HTTP status of a change-set error code.
func bundleStatus(code string) int {
	switch code {
	case bundle.CodePlanBlocked, bundle.CodeStepFailed:
		return http.StatusUnprocessableEntity
	case bundle.CodeSamePrincipal:
		return http.StatusForbidden
	default: // change_set_stale, change_set_open
		return http.StatusConflict
	}
}

func (s *Server) planChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in bundle.Request
	if err := decode(r, &in); err != nil {
		return err
	}
	cs, err := s.bundles.Plan(r.Context(), actor(c), in)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if cs.State != "" {
		code = http.StatusCreated
	}
	writeJSON(w, code, cs)
	return nil
}

func (s *Server) listChangeSets(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return badRequest{"limit must be a number"}
		}
		limit = n
	}
	list, err := s.bundles.List(r.Context(), actor(c), r.URL.Query().Get("bundle"), limit)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"change_sets": list})
	}
	return err
}

func (s *Server) getChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Get(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) submitChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Submit(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) approveChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	cs, err := s.bundles.Approve(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) rejectChangeSet(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	cs, err := s.bundles.Reject(r.Context(), actor(c), id, in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, cs)
	}
	return err
}

func (s *Server) listBundles(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	list, err := s.bundles.Bundles(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"bundles": list})
	}
	return err
}

func (s *Server) bundleDrift(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	d, err := s.bundles.Drift(r.Context(), actor(c), r.PathValue("name"))
	if err == nil {
		writeJSON(w, http.StatusOK, d)
	}
	return err
}
