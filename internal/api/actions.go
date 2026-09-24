package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"eacp/internal/action"
	"eacp/internal/identity"
)

const (
	maxWait      = 60 * time.Second
	waitInterval = 100 * time.Millisecond
)

// WithActions replaces the Action API engine (the default evaluates with the
// local PDP and the default admission limits).
func (s *Server) WithActions(e *action.Engine) *Server {
	s.actions = e
	return s
}

// either wraps h for agent keys and for principal keys holding one of roles
// (nil: any principal).
func (s *Server) either(roles []string, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if c.Kind != identity.KindAgent && (c.Kind != identity.KindPrincipal || (roles != nil && !c.HasRole(roles...))) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		s.finish(w, r, h(w, r, c))
	})
}

func actionActor(c identity.Caller) action.Actor {
	if c.Kind == identity.KindAgent {
		return action.Agent(c.TenantID, c.AgentID, c.AgentVersionID)
	}
	return action.Principal(c.TenantID, c.PrincipalID)
}

// waitParam parses ?wait= as a Go duration ("5s") or whole seconds ("5").
func waitParam(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		n, nerr := strconv.Atoi(raw)
		if nerr != nil {
			return 0, badRequest{"wait must be a duration such as 5s, or whole seconds"}
		}
		d = time.Duration(n) * time.Second
	}
	if d < 0 || d > maxWait {
		return 0, badRequest{"wait must be between 0s and 60s"}
	}
	return d, nil
}

// await polls the action until it is terminal, the wait ends or the client
// goes away. An agent's own wait also releases its approved action (T10-T13)
// instead of leaving that to the sweeper.
func (s *Server) await(r *http.Request, a action.Actor, v action.View, wait time.Duration) action.View {
	if wait <= 0 || v.Terminal() {
		return v
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(waitInterval)
	defer tick.Stop()
	// One failed release attempt (e.g. the PDP is down) hands retries to the
	// sweeper, so a waiting client never polls the PDP.
	advance := a.AgentVersionID != uuid.Nil
	for !v.Terminal() {
		select {
		case <-r.Context().Done():
			return v
		case <-deadline.C:
			return v
		case <-tick.C:
		}
		var next action.View
		var err error
		if advance && v.State == "AUTHORIZED" {
			next, err = s.actions.Advance(r.Context(), a, v.ID)
			advance = err == nil
		} else {
			next, err = s.actions.Read(r.Context(), a, v.ID)
		}
		if next.ID != v.ID {
			if err != nil && r.Context().Err() == nil {
				s.log.WarnContext(r.Context(), "action wait", "action", v.ID.String(), "err", err)
			}
			continue
		}
		v = next
	}
	return v
}

// writeAction answers 200 for a terminal action and 202 for one in progress.
func writeAction(w http.ResponseWriter, v action.View) {
	code := http.StatusAccepted
	if v.Terminal() {
		code = http.StatusOK
	}
	writeJSON(w, code, v)
}

type submitBody struct {
	Subject           string          `json:"subject"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Tool              string          `json:"tool"`
	ToolSchemaVersion string          `json:"tool_schema_version"`
	Resource          string          `json:"resource"`
	Payload           json.RawMessage `json:"payload"`
	LifetimeSeconds   int             `json:"lifetime_seconds"`
}

// submitAction is POST /v1/actions (ADR-004 T1). The Idempotency-Key header
// is required; a replay returns the existing action and a different request
// under the same key is 409.
func (s *Server) submitAction(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return badRequest{"the Idempotency-Key header is required"}
	}
	wait, err := waitParam(r)
	if err != nil {
		return err
	}
	var in submitBody
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.LifetimeSeconds < 0 {
		return badRequest{"lifetime_seconds must be positive"}
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(r.Context(), carrier)

	a := actionActor(c)
	v, err := s.actions.Submit(r.Context(), a, action.Submission{
		IdempotencyKey: key, Subject: in.Subject, Operation: in.Operation, Target: in.Target, Tool: in.Tool,
		ToolSchemaVersion: in.ToolSchemaVersion, Resource: in.Resource, Payload: in.Payload,
		Lifetime: time.Duration(in.LifetimeSeconds) * time.Second, Traceparent: carrier.Get("traceparent"),
	})
	switch {
	case errors.Is(err, action.ErrIdempotencyConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "idempotency_conflict",
			"detail": "this Idempotency-Key was used for a different request"})
		return nil
	case errors.Is(err, action.ErrAdmission):
		scope := "tenant"
		var a *action.AdmissionError
		if errors.As(err, &a) {
			scope = a.Scope
		}
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "admission_limit", "scope": scope})
		return nil
	case errors.Is(err, action.ErrGovernanceUnavailable):
		// The action is persisted and stays RECEIVED; resubmitting the same
		// key (or the sweeper) evaluates it again. Nothing became executable.
		w.Header().Set("Retry-After", "5")
		body := struct {
			action.View
			Error    string `json:"error"`
			ActionID string `json:"action_id,omitempty"`
		}{View: v, Error: "governance_unavailable"}
		if v.ID != uuid.Nil {
			body.ActionID = v.ID.String()
		}
		writeJSON(w, http.StatusServiceUnavailable, body)
		return nil
	case err != nil:
		return err
	}
	writeAction(w, s.await(r, a, v, wait))
	return nil
}

// getAction is GET /v1/actions/{id}: an agent sees its own agent's actions,
// operators and auditors see the tenant's.
func (s *Server) getAction(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	wait, err := waitParam(r)
	if err != nil {
		return err
	}
	a := actionActor(c)
	v, err := s.actions.Read(r.Context(), a, id)
	if err != nil {
		return err
	}
	writeAction(w, s.await(r, a, v, wait))
	return nil
}

// cancelAction is POST /v1/actions/{id}/cancel. The database decides who
// may cancel: the submitting agent, the subject or an operator.
func (s *Server) cancelAction(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in reasonBody
	if err := decode(r, &in); err != nil {
		return err
	}
	v, err := s.actions.Cancel(r.Context(), actionActor(c), id, in.Reason)
	if err == nil {
		writeAction(w, v)
	}
	return err
}

// listActions is GET /v1/actions?state=&limit= for operators and auditors:
// the tenant's actions in one state, oldest first (e.g. the
// NEEDS_HUMAN_RESOLUTION queue).
func (s *Server) listActions(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	state := r.URL.Query().Get("state")
	if state == "" {
		return badRequest{"state is required"}
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return badRequest{"limit must be between 1 and 200"}
		}
		limit = n
	}
	views, err := s.actions.List(r.Context(), c.TenantID, state, limit)
	if err != nil {
		return err
	}
	if views == nil {
		views = []action.View{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": views})
	return nil
}

// actionEvidence is GET /v1/actions/{id}/evidence for operators and
// auditors: every attempt, reconciliation check and resolution.
func (s *Server) actionEvidence(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ev, err := s.actions.Evidence(r.Context(), c.TenantID, id)
	if err == nil {
		writeJSON(w, http.StatusOK, ev)
	}
	return err
}

type resolveBody struct {
	Outcome           string `json:"outcome"`
	Reason            string `json:"reason"`
	Evidence          string `json:"evidence"`
	ExternalReference string `json:"external_reference"`
}

// writeResolution answers 200 when the resolution moved the action and 202
// for a retry proposal that awaits a second operator.
func writeResolution(w http.ResponseWriter, v action.View, res action.ResolutionView) {
	code := http.StatusOK
	if res.State == "PROPOSED" {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"action": v, "resolution": res})
}

// resolveAction is POST /v1/actions/{id}/resolutions (operators, ADR-004
// T35-T37): succeeded or failed applies at once; retry is proposed and
// needs a second operator's confirmation. The database enforces separation
// of duties and the evidence each outcome needs.
func (s *Server) resolveAction(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in resolveBody
	if err := decode(r, &in); err != nil {
		return err
	}
	v, res, err := s.actions.Resolve(r.Context(), actionActor(c), id, action.Resolution{Outcome: in.Outcome,
		Reason: in.Reason, Evidence: in.Evidence, ExternalReference: in.ExternalReference})
	if err == nil {
		writeResolution(w, v, res)
	}
	return err
}

// decideResolution is POST /v1/actions/{id}/resolutions/{rid}/confirm or
// /withdraw (operators). Only a second, distinct operator confirms a retry.
func (s *Server) decideResolution(confirm bool) handler {
	return func(w http.ResponseWriter, r *http.Request, c identity.Caller) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		rid, err := pathID(r, "rid")
		if err != nil {
			return err
		}
		var in reasonBody
		if err := decode(r, &in); err != nil {
			return err
		}
		v, res, err := s.actions.DecideResolution(r.Context(), actionActor(c), id, rid, confirm, in.Reason)
		if err == nil {
			writeResolution(w, v, res)
		}
		return err
	}
}
