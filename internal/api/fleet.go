package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/fleet"
	"github.com/atipongsena/eacp/internal/identity"
)

// fleetReader may see the fleet view: those who contain, grant or audit.
var fleetReader = []string{"operator", "registry_approver", "auditor"}

func (s *Server) registerFleet(mux *http.ServeMux) {
	mux.Handle("GET /v1/fleet/agents", s.principal(fleetReader, s.fleetAgents))
	mux.Handle("GET /v1/fleet/health", s.principal(fleetReader, s.fleetHealth))
	mux.Handle("POST /v1/fleet/operations", s.principal(containment, s.fleetApply))
	mux.Handle("GET /v1/fleet/operations/{id}", s.principal(fleetReader, s.fleetOperation))
}

func fleetFilter(r *http.Request) (fleet.Filter, error) {
	q := r.URL.Query()
	f := fleet.Filter{Environment: q.Get("environment"), RiskClass: q.Get("risk_class"), Health: q.Get("health")}
	if raw := q.Get("owner_group_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return f, badRequest{"owner_group_id must be a UUID"}
		}
		f.OwnerGroupID = id
	}
	if raw := q.Get("window"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return f, badRequest{"window must be a duration such as 24h"}
		}
		f.Window = d
	}
	return f, nil
}

func (s *Server) fleetAgents(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	f, err := fleetFilter(r)
	if err != nil {
		return err
	}
	agents, err := s.fleet.Agents(r.Context(), actor(c), f)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
	}
	return err
}

func (s *Server) fleetHealth(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	f, err := fleetFilter(r)
	if err != nil {
		return err
	}
	sum, err := s.fleet.Health(r.Context(), actor(c), f)
	if err == nil {
		writeJSON(w, http.StatusOK, sum)
	}
	return err
}

func (s *Server) fleetApply(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in fleet.Request
	if err := decode(r, &in); err != nil {
		return err
	}
	op, err := s.fleet.Apply(r.Context(), actor(c), in)
	if err != nil {
		return err
	}
	code := http.StatusCreated
	if op.DryRun {
		code = http.StatusOK
	}
	writeJSON(w, code, op)
	return nil
}

func (s *Server) fleetOperation(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	op, err := s.fleet.Operation(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, op)
	}
	return err
}
