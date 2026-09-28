package api

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/incident"
)

var (
	incidentWorker = []string{"operator", "admin"}
	incidentReader = []string{"operator", "auditor", "admin"}
)

func (s *Server) registerIncidents(mux *http.ServeMux) {
	p := s.principal
	mux.Handle("GET /v1/incidents", p(incidentReader, s.listIncidents))
	mux.Handle("GET /v1/incidents/{id}", p(incidentReader, s.getIncident))
	mux.Handle("POST /v1/incidents", p(incidentWorker, s.openIncident))
	mux.Handle("POST /v1/incidents/{id}/acknowledge", p(incidentWorker, s.acknowledgeIncident))
	mux.Handle("POST /v1/incidents/{id}/assign", p(incidentWorker, s.assignIncident))
	mux.Handle("POST /v1/incidents/{id}/notes", p(incidentWorker, s.noteIncident))
	mux.Handle("POST /v1/incidents/{id}/links", p(incidentWorker, s.linkIncident))
	mux.Handle("POST /v1/incidents/{id}/resolve", p(incidentWorker, s.resolveIncident))
	mux.Handle("GET /v1/soc/summary", p(incidentReader, s.socSummary))
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	q := r.URL.Query()
	f := incident.Filter{State: q.Get("state"), Severity: q.Get("severity"), Kind: q.Get("kind")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			return badRequest{"limit must be between 1 and 500"}
		}
		f.Limit = n
	}
	list, err := s.incidents.List(r.Context(), actor(c), f)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"incidents": list})
	}
	return err
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	in, err := s.incidents.Get(r.Context(), actor(c), id)
	if err == nil {
		writeJSON(w, http.StatusOK, in)
	}
	return err
}

func (s *Server) openIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in incident.NewIncident
	if err := decode(r, &in); err != nil {
		return err
	}
	out, err := s.incidents.Open(r.Context(), actor(c), in)
	return created(w, out, err)
}

// incidentMove decodes the body into v, then runs fn on the path's incident.
func (s *Server) incidentMove(w http.ResponseWriter, r *http.Request, status int, v any,
	fn func(id uuid.UUID) (incident.Incident, error)) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := decode(r, v); err != nil {
		return err
	}
	out, err := fn(id)
	if err == nil {
		writeJSON(w, status, out)
	}
	return err
}

func (s *Server) acknowledgeIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Reason string `json:"reason"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Acknowledge(r.Context(), actor(c), id, in.Reason)
	})
}

func (s *Server) assignIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		AssigneeID *uuid.UUID `json:"assignee_id"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Assign(r.Context(), actor(c), id, in.AssigneeID)
	})
}

func (s *Server) noteIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Text string `json:"text"`
	}
	return s.incidentMove(w, r, http.StatusCreated, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Note(r.Context(), actor(c), id, in.Text)
	})
}

func (s *Server) linkIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Kind string    `json:"kind"`
		ID   uuid.UUID `json:"id"`
	}
	return s.incidentMove(w, r, http.StatusCreated, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Link(r.Context(), actor(c), id, in.Kind, in.ID)
	})
}

func (s *Server) resolveIncident(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in struct {
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
	}
	return s.incidentMove(w, r, http.StatusOK, &in, func(id uuid.UUID) (incident.Incident, error) {
		return s.incidents.Resolve(r.Context(), actor(c), id, in.Resolution, in.Reason)
	})
}

func (s *Server) socSummary(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	sum, err := s.incidents.Summary(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, sum)
	}
	return err
}
