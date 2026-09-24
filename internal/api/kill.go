package api

import (
	"net/http"

	"eacp/internal/identity"
	"eacp/internal/kill"
)

func (s *Server) registerKill(mux *http.ServeMux) {
	mux.Handle("POST /v1/killswitch", s.principal(operator, s.setKill))
	mux.Handle("GET /v1/killswitch", s.principal(actionReader, s.listKills))
}

func (s *Server) setKill(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in kill.Change
	if err := decode(r, &in); err != nil {
		return err
	}
	state, err := s.kills.Set(r.Context(), actor(c), in)
	if err == nil {
		writeJSON(w, http.StatusOK, state)
	}
	return err
}

func (s *Server) listKills(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	states, err := s.kills.List(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"kills": states})
	}
	return err
}
