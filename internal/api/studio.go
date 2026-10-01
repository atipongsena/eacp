package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/atipongsena/eacp/internal/identity"
	"github.com/atipongsena/eacp/internal/studio"
)

// Agent Studio (Phase 27a-1, ADR-033). Authors save; approvers decide;
// PostgreSQL enforces every rule again.
var (
	studioAuthor = []string{"studio_author"}
	studioReader = []string{"studio_author", "registry_approver", "auditor"}
	// Studio runs (Phase 27a-2): these read every run, never an answer.
	studioRunReader = []string{"registry_approver", "operator", "auditor"}
	studioRuntime   = []string{"studio_runtime"}
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

	mux.Handle("POST /v1/studio/agents/{id}/runs", p(anyPrincipal, s.studioStartRun))
	mux.Handle("GET /v1/studio/runs/{id}", p(anyPrincipal, s.studioRun))
	mux.Handle("POST /v1/studio/credentials/revoke-all", p(operator, s.studioRevokeAll))
	mux.Handle("POST /v1/studio/runtime/claims", p(studioRuntime, s.studioClaim))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/heartbeat", p(studioRuntime, s.studioHeartbeat))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/steps", p(studioRuntime, s.studioStep))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/finish", p(studioRuntime, s.studioFinish))
	mux.Handle("POST /v1/studio/versions/{id}/previews", p(studioAuthor, s.studioPreview))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/nodes/begin", p(studioRuntime, s.studioNode("begin")))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/nodes/complete", p(studioRuntime, s.studioNode("complete")))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/nodes/output", p(studioRuntime, s.studioNode("output")))
	mux.Handle("POST /v1/studio/runtime/runs/{id}/llm/begin", p(studioRuntime, s.studioNode("llm")))
	mux.Handle("GET /v1/studio/runtime/credentials", p(studioRuntime, s.studioDue))
	mux.Handle("POST /v1/studio/runtime/credentials", p(studioRuntime, s.studioPropose))
	s.registerStudioHub(mux)
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
		Definition        json.RawMessage `json:"definition"`
		ExpectedVersionID uuid.UUID       `json:"expected_version_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.studio.AddVersionChecked(r.Context(), actor(c), agent, in.Definition, in.ExpectedVersionID)
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
	if err != nil {
		return err
	}
	keys, err := s.studio.KeyRequests(r.Context(), actor(c))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"requests": requests, "keys": keys})
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

// ------------------------------------------------------------------ runs

func (s *Server) studioStartRun(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	agent, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Inputs json.RawMessage `json:"inputs"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	run, err := s.studio.Start(r.Context(), actor(c), agent, in.Inputs)
	return created(w, run, err)
}

func (s *Server) studioRun(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	run, err := s.studio.Run(r.Context(), actor(c), id, c.HasRole(studioRunReader...))
	if err == nil {
		writeJSON(w, http.StatusOK, run)
	}
	return err
}

func (s *Server) studioRevokeAll(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	n, err := s.studio.RevokeAll(r.Context(), actor(c), in.Reason)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]int{"revoked": n})
	}
	return err
}

// --------------------------------------------------------------- runtime

func (s *Server) studioClaim(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in studio.ClaimRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	runs, err := s.studio.Claim(r.Context(), actor(c), in)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
	}
	return err
}

func (s *Server) studioHeartbeat(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		studio.Lease
		LeaseSeconds int `json:"lease_seconds"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	status, err := s.studio.Heartbeat(r.Context(), actor(c), id, in.Lease, in.LeaseSeconds)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"version_active": status == studio.BeatActive,
			"killed": status == studio.BeatKilled})
	}
	return err
}

func (s *Server) studioStep(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		studio.Lease
		Index    int       `json:"index"`
		ActionID uuid.UUID `json:"action_id"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.studio.Step(r.Context(), actor(c), id, in.Lease, in.Index, in.ActionID))
}

func (s *Server) studioFinish(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		studio.Lease
		State  string  `json:"state"`
		Answer *string `json:"answer"`
		Reason *string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.studio.Finish(r.Context(), actor(c), id, in.Lease, in.State, in.Answer, in.Reason))
}

func (s *Server) studioDue(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	master := r.URL.Query().Get("master_version")
	if master == "" {
		return badRequest{"master_version is required"}
	}
	due, err := s.studio.DueCredentials(r.Context(), actor(c), master)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"due": due})
	}
	return err
}

func (s *Server) studioPropose(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	var in studio.Proposal
	if err := decode(r, &in); err != nil {
		return err
	}
	return noContent(w, s.studio.Propose(r.Context(), actor(c), in))
}
