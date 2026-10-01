package api

import (
	"encoding/json"
	"net/http"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/studio"
)

func (s *Server) studioPreview(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Inputs  json.RawMessage `json:"inputs"`
		Samples json.RawMessage `json:"samples"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	run, err := s.studio.Preview(r.Context(), actor(c), id, in.Inputs, in.Samples)
	return created(w, run, err)
}

func (s *Server) studioNode(operation string) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		var in studio.NodeRequest
		if err := decode(r, &in); err != nil {
			return err
		}
		if operation == "complete" {
			return noContent(w, s.studio.CompleteNode(r.Context(), actor(c), id, in))
		}
		if operation == "action" {
			return noContent(w, s.studio.NodeAction(r.Context(), actor(c), id, in))
		}
		var out json.RawMessage
		if operation == "output" {
			out, err = s.studio.NodeOutput(r.Context(), actor(c), id, in)
		} else {
			out, err = s.studio.BeginNode(r.Context(), actor(c), id, in, operation == "llm")
		}
		if err == nil {
			writeJSON(w, http.StatusOK, out)
		}
		return err
	}
}
